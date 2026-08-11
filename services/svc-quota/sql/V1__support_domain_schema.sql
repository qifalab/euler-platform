-- =============================================================================
-- 支撑域 DDL — svc-quota / svc-notify / svc-audit
--
-- Sharding: account_id single key (04§6.3). Audit detail lives in ClickHouse;
-- MySQL holds only the chain checkpoints, because the audit write path is
-- high-volume append-only and the query path is analytical.
--
-- Source of truth: 03-backend-services.md §4.3.2 (quota), §4.4.2 (notify),
-- §4.4.3 (audit); 07-security.md §6 (audit schema, chain hashing).
-- =============================================================================

-- =============================================================================
-- svc-quota — resource_db
-- =============================================================================

-- -----------------------------------------------------------------------------
-- quota_definition — 配额定义(全局广播表)
-- -----------------------------------------------------------------------------
CREATE TABLE `quota_definition` (
  `quota_code`    VARCHAR(64) NOT NULL COMMENT 'quota_scecs_instance 等',
  `product_code`  VARCHAR(32) NOT NULL,
  `default_value` INT NOT NULL COMMENT '账户默认上限',
  `scope`         VARCHAR(16) NOT NULL DEFAULT 'REGION' COMMENT 'GLOBAL 跨地域合计 / REGION 分地域独立',
  `adjustable`    TINYINT NOT NULL DEFAULT 1 COMMENT '1可提额(转工单审批) 0硬上限',
  `unit`          VARCHAR(16) DEFAULT NULL COMMENT '个/核/GB',
  `created_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`quota_code`),
  KEY `idx_product` (`product_code`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='配额定义';

-- -----------------------------------------------------------------------------
-- quota_usage — 配额用量
--
-- used + occupying is what counts against hard_limit. Ignoring occupying is
-- precisely how two concurrent orders both get the last slot: each sees
-- committed usage below the limit and neither knows about the other's
-- in-flight reservation.
--
-- version drives the optimistic lock. 03§4.3.2 is explicit that the DB lock is
-- authoritative and Redis only accelerates the read: a Redis-authoritative
-- counter loses state on failover, and quota that resets on a cache restart is
-- quota that can be spent twice.
-- -----------------------------------------------------------------------------
CREATE TABLE `quota_usage` (
  `id`         BIGINT UNSIGNED NOT NULL,
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `quota_code` VARCHAR(64) NOT NULL,
  `region`     VARCHAR(32) NOT NULL DEFAULT '*' COMMENT 'GLOBAL 作用域固定为 *',
  `used`       INT NOT NULL DEFAULT 0 COMMENT '已提交用量:资源确实存在',
  `occupying`  INT NOT NULL DEFAULT 0 COMMENT '预留未提交:履约中的订单;不计入则超卖',
  `hard_limit` INT NOT NULL COMMENT '账户级上限,缺省取 quota_definition.default_value',
  `version`    INT NOT NULL DEFAULT 0 COMMENT '乐观锁;DB 为准,Redis 仅加速读',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_acc_quota_region` (`account_id`,`quota_code`,`region`),
  KEY `idx_warn` (`account_id`,`used`,`hard_limit`) COMMENT '80% 预警扫描',
  CONSTRAINT `ck_used_non_negative` CHECK (`used` >= 0),
  CONSTRAINT `ck_occupying_non_negative` CHECK (`occupying` >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='配额用量;计数器为负会凭空放出容量,故加 CHECK 约束';

-- -----------------------------------------------------------------------------
-- quota_token — 两阶段占用令牌
--
-- The TTL is the load-bearing part. A saga that dies between occupy and commit
-- would otherwise hold capacity forever, and quota that is neither used nor
-- sellable is the worst of both worlds: customers are refused while the
-- hardware sits idle and nothing reports a problem.
--
-- expires_at is swept back automatically (03§4.3.2 占用令牌带 TTL 防止悬挂).
-- -----------------------------------------------------------------------------
CREATE TABLE `quota_token` (
  `token_id`   VARCHAR(64) NOT NULL,
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `quota_code` VARCHAR(64) NOT NULL,
  `region`     VARCHAR(32) NOT NULL DEFAULT '*',
  `amount`     INT NOT NULL,
  `biz_key`    VARCHAR(64) NOT NULL COMMENT 'order_id;泄漏的令牌据此追溯到放弃它的订单',
  `expires_at` DATETIME NOT NULL COMMENT '默认 15 分钟,与 CREATING 超时对齐',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`token_id`),
  KEY `idx_expires` (`expires_at`) COMMENT '悬挂令牌清扫索引',
  KEY `idx_acc_quota` (`account_id`,`quota_code`,`region`),
  KEY `idx_biz` (`biz_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='配额占用令牌;超时自动回收';

-- -----------------------------------------------------------------------------
-- 对账视图:配额计数漂移
--
-- 03§8.5 配额↔资源对账. Quota is a DERIVED counter; the resource ledger is the
-- thing being counted. When they disagree, the ledger is right and the counter
-- is corrected — a counter that drifts high refuses customers who should be
-- served, one that drifts low oversells.
--
-- Cross-database in the logical model; the reconciliation job reads both
-- through their services rather than joining across shards.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE VIEW `v_quota_dangling_tokens` AS
SELECT
  t.token_id,
  t.account_id,
  t.quota_code,
  t.region,
  t.amount,
  t.biz_key,
  t.expires_at,
  TIMESTAMPDIFF(MINUTE, t.expires_at, UTC_TIMESTAMP()) AS overdue_minutes
FROM quota_token t
WHERE t.expires_at < UTC_TIMESTAMP();

-- =============================================================================
-- svc-notify — workflow_db
-- =============================================================================

-- -----------------------------------------------------------------------------
-- notification — 通知记录
--
-- 03§4.4.2: 欠费催收/到期提醒/释放预告 three classes MUST be persisted and
-- queryable. 对标启示 8 gives the reason — when a customer's data is deleted,
-- the only acceptable answer to "you never told me" is a queryable record
-- showing when the warning went out and through which channel.
--
-- Persist-before-send: a message that crashes mid-delivery is still on record
-- as attempted, which is what lets the sweeper retry it and lets a support
-- agent see that the platform tried.
-- -----------------------------------------------------------------------------
CREATE TABLE `notification` (
  `notification_id` VARCHAR(64) NOT NULL,
  `account_id`      BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `class`           VARCHAR(24) NOT NULL COMMENT 'ARREARS/EXPIRY/RELEASE_WARNING 为信任关键三类,其余 ALERT/ORDER/MARKETING',
  `template_id`     VARCHAR(64) NOT NULL,
  `params_json`     JSON DEFAULT NULL,
  `biz_key`         VARCHAR(64) NOT NULL COMMENT 'resource_id/account_id;信任关键类必填,否则无法举证',
  `channels`        VARCHAR(128) NOT NULL COMMENT '逗号分隔 IN_APP,SMS,EMAIL,WEBHOOK',
  `status`          VARCHAR(16) NOT NULL DEFAULT 'PENDING' COMMENT 'PENDING/SENT/FAILED/SUPPRESSED',
  `attempts`        INT NOT NULL DEFAULT 0,
  `last_error`      VARCHAR(512) DEFAULT NULL,
  `sent_at`         DATETIME DEFAULT NULL,
  `created_at`      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`notification_id`),
  KEY `idx_acc_class_biz` (`account_id`,`class`,`biz_key`) COMMENT '释放前校验"是否已通知"的主查询',
  KEY `idx_acc_created` (`account_id`,`created_at`) COMMENT '限流窗口计数',
  KEY `idx_retry` (`status`,`attempts`,`created_at`) COMMENT '失败重试扫描'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='通知记录;信任关键三类必须落库可查(03§4.4.2/对标启示8)';

-- -----------------------------------------------------------------------------
-- notification_delivery — 逐渠道投递结果
--
-- provider_message_id is the dispute evidence: it is what lets the platform
-- demonstrate to a customer (or a regulator) that the SMS gateway accepted the
-- message at a specific time.
-- -----------------------------------------------------------------------------
CREATE TABLE `notification_delivery` (
  `id`                  BIGINT UNSIGNED NOT NULL,
  `notification_id`     VARCHAR(64) NOT NULL,
  `account_id`          BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键',
  `channel`             VARCHAR(16) NOT NULL,
  `success`             TINYINT NOT NULL,
  `provider`            VARCHAR(64) DEFAULT NULL COMMENT '短信网关/邮件中继',
  `provider_message_id` VARCHAR(128) DEFAULT NULL COMMENT '渠道侧流水号;争议举证依据',
  `error_msg`           VARCHAR(512) DEFAULT NULL,
  `sent_at`             DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_notification` (`notification_id`),
  KEY `idx_acc_channel` (`account_id`,`channel`,`sent_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='逐渠道投递结果';

-- -----------------------------------------------------------------------------
-- 视图:信任关键通知未送达
--
-- These are the notifications whose non-delivery blocks a destructive action.
-- A release warning sitting here means a resource cannot be released until
-- someone resolves it — which is the intended behaviour, not a bug.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE VIEW `v_notify_trust_critical_undelivered` AS
SELECT
  n.notification_id,
  n.account_id,
  n.class,
  n.biz_key,
  n.status,
  n.attempts,
  n.last_error,
  n.created_at
FROM notification n
WHERE n.class IN ('ARREARS','EXPIRY','RELEASE_WARNING')
  AND n.status <> 'SENT';

-- =============================================================================
-- svc-audit — infra_db (chain checkpoints only; detail in ClickHouse)
-- =============================================================================

-- -----------------------------------------------------------------------------
-- audit_chain_checkpoint — 审计链检查点
--
-- The full event stream lives in ClickHouse (partitioned by day, ≥180-day hot
-- retention plus MinIO cold backup — adjudication S24). MySQL keeps only the
-- per-tenant chain head and sequence.
--
-- Why a checkpoint at all: a hash chain proves internal consistency, but an
-- attacker who truncates the tail and stops there leaves a shorter chain that
-- still verifies on its own. Periodically recording the head elsewhere is what
-- makes truncation detectable — the stored head no longer matches the chain.
-- -----------------------------------------------------------------------------
CREATE TABLE `audit_chain_checkpoint` (
  `account_id`     BIGINT UNSIGNED NOT NULL COMMENT '每租户一条链(07§6.2)',
  `chain_head`     CHAR(64) NOT NULL COMMENT '最新事件的 chain_hash',
  `sequence`       BIGINT UNSIGNED NOT NULL COMMENT '已记录事件数;缺口即被删除',
  `checkpoint_at`  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`account_id`),
  KEY `idx_checkpoint` (`checkpoint_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='审计链检查点;截断攻击靠"存储的头与链不符"发现';

-- -----------------------------------------------------------------------------
-- ClickHouse 审计事件表(DDL 参考,实际在 CK 集群执行)
--
-- 07§6.2: audit DB accounts are separate from business DB accounts (三权分立);
-- developers hold no edit or delete permission. The hash chain makes tampering
-- detectable even when permissions fail, which is the point — the people most
-- motivated to alter an audit record are often the ones who can change
-- permissions.
--
-- CREATE TABLE audit.action_log ON CLUSTER obs_cluster (
--   event_id        String,
--   event_time      DateTime64(3),
--   account_id      UInt64,
--   event_source    String,
--   event_name      String,
--   source_ip       String,
--   user_agent      String,
--   identity_type   LowCardinality(String),
--   principal       String,
--   ak_id           String,          -- masked SC****3F, never full key material
--   mfa_present     UInt8,
--   resources       Array(String),
--   decision        LowCardinality(String),  -- allow/deny; denials are audited too
--   decision_number String,
--   request_params  String,          -- JSON, secrets redacted at write time
--   response_code   UInt16,
--   trace_id        String,
--   prev_hash       FixedString(64),
--   chain_hash      FixedString(64),
--   sequence        UInt64
-- ) ENGINE = ReplicatedMergeTree()
--   PARTITION BY toYYYYMMDD(event_time)
--   ORDER BY (account_id, sequence)   -- chain order per tenant
--   TTL event_time + INTERVAL 180 DAY TO VOLUME 'cold',
--       event_time + INTERVAL 18 MONTH DELETE;
-- =============================================================================
