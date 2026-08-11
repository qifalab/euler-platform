-- =============================================================================
-- svc-orchestrator DDL — resource_db (资源台账与编排)
--
-- Sharding: account_id single key, 8 库 × 16 表 (04§6.3).
--
-- Adjudication S21: svc-orchestrator is the SOLE owner of the resource
-- lifecycle. resource_instance below is the single ledger; 06§4.5's
-- product_instance table is abolished. The K8s CR status.phase is an
-- observation field, never the source of truth — a controller reporting
-- "Running" is evidence, not authority.
--
-- Source of truth: 03-backend-services.md §6.2 (resource_instance),
-- §4.3.3 (workflow), 06-k8s §4.5 (provision_task).
-- =============================================================================

-- -----------------------------------------------------------------------------
-- resource_instance — 全产品资源台账
--
-- Every sellable resource on the platform is one row here, whatever the
-- product. Product-private attributes go to resource_instance_attr or the CR.
-- That uniformity is what lets the console list, the bill, the audit trail and
-- the quota counter work without a per-product special case each time a new
-- product ships (架构原则 2 一切皆资源).
--
-- region is a plain column, NOT part of the shard key (adjudication C5/S12
-- rejected region+account_id composite routing). Console queries are
-- overwhelmingly "this tenant's resources", which a single-key shard answers
-- from one shard; adding region to the key would scatter them.
-- -----------------------------------------------------------------------------
CREATE TABLE `resource_instance` (
  `resource_id`   VARCHAR(64) NOT NULL COMMENT '{productCode}-{regionId}-{分片因子2位}-{随机8位},内嵌分片因子供反查(04§6.6)',
  `account_id`    BIGINT UNSIGNED NOT NULL COMMENT '分片键(≡uid≡user_id≡tenant_id)',
  `project_id`    BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '资源组;缺省 default(07§3.6)',
  `product_code`  VARCHAR(32) NOT NULL,
  `resource_type` VARCHAR(64) NOT NULL,
  `region`        VARCHAR(32) NOT NULL COMMENT 'Day1 建模,一期单地域部署(对标启示6)',
  `zone`          VARCHAR(32) DEFAULT NULL,
  `charge_type`   TINYINT NOT NULL COMMENT '1包年包月 2按量;3资源包 模型预留',
  `status`        VARCHAR(32) NOT NULL COMMENT 'INIT/CREATING/RUNNING/STOPPED/UPGRADING/LOCKED/EXPIRED/RELEASING/RELEASED/CREATE_FAILED',
  `spec_code`     VARCHAR(64) DEFAULT NULL COMMENT '规格快照',
  `order_id`      BIGINT UNSIGNED DEFAULT NULL,
  `billing_start` DATETIME DEFAULT NULL COMMENT '计费起点=首次 RUNNING 时刻,永不回拨;否则可 stop/start 刷掉账单',
  `expired_at`    DATETIME DEFAULT NULL COMMENT '包年包月到期时间',
  `locked_at`     DATETIME DEFAULT NULL COMMENT '欠费锁定时间;30天保留期由此计',
  `released_at`   DATETIME DEFAULT NULL,
  `final_notice_at` DATETIME DEFAULT NULL COMMENT '释放前终版通知发送时间;为空不得释放(03§5.4)',
  `version`       INT NOT NULL DEFAULT 0 COMMENT '状态机乐观锁;迁移带 WHERE status=? AND version=?',
  `created_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`resource_id`),
  KEY `idx_acc_prod` (`account_id`,`product_code`,`status`) COMMENT '控制台资源列表主查询',
  KEY `idx_status_charge` (`status`,`charge_type`) COMMENT '欠费扫描/到期扫描',
  KEY `idx_status_updated` (`status`,`updated_at`) COMMENT '中间态超时扫描(CREATING 15min 等)',
  KEY `idx_expired` (`expired_at`,`status`) COMMENT '到期与续费提醒扫描',
  KEY `idx_locked` (`locked_at`,`status`) COMMENT '锁定保留期到期扫描',
  KEY `idx_order` (`order_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='资源台账;状态机唯一写入口=svc-orchestrator,K8s phase 仅作观测字段(裁决S21)';

-- -----------------------------------------------------------------------------
-- resource_instance_attr — 产品私有属性 KV 扩展
-- Deep product-private state lives in the controller's CR; this table holds
-- what the console and billing need to show without calling the product.
-- -----------------------------------------------------------------------------
CREATE TABLE `resource_instance_attr` (
  `resource_id` VARCHAR(64) NOT NULL,
  `account_id`  BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键;禁止跨库 JOIN',
  `attr_key`    VARCHAR(64) NOT NULL,
  `attr_value`  VARCHAR(1024) DEFAULT NULL,
  `updated_at`  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`resource_id`,`attr_key`),
  KEY `idx_account` (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='产品私有属性KV';

-- -----------------------------------------------------------------------------
-- resource_state_log — 状态迁移流水(只增不改)
--
-- "Why is this resource in this state" must be answerable months later without
-- replaying Kafka. For a resource that was released, this log is the only
-- remaining explanation of what happened to the customer's data.
-- -----------------------------------------------------------------------------
CREATE TABLE `resource_state_log` (
  `id`          BIGINT UNSIGNED NOT NULL,
  `resource_id` VARCHAR(64) NOT NULL,
  `account_id`  BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键',
  `from_status` VARCHAR(32) DEFAULT NULL COMMENT 'NULL 表示创建',
  `to_status`   VARCHAR(32) NOT NULL,
  `reason`      VARCHAR(256) DEFAULT NULL COMMENT '回调/超时/欠费/用户操作/运营干预',
  `operator`    VARCHAR(64) NOT NULL DEFAULT 'system',
  `occurred_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_resource_time` (`resource_id`,`occurred_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='资源状态迁移流水;释放后这是唯一的交代';

-- -----------------------------------------------------------------------------
-- provision_task — 履约任务 (06§4.5)
--
-- svc-orchestrator dispatches to rc-* via SYNCHRONOUS gRPC ApplyResource
-- (adjudication S21 rejected Kafka as the main dispatch channel). This table
-- records the dispatch so a lost callback can be retried and a stuck task can
-- be found; the Kafka topics cloud.resource.provision.{task,status} are async
-- callback and retry channels only.
-- -----------------------------------------------------------------------------
CREATE TABLE `provision_task` (
  `task_id`     BIGINT UNSIGNED NOT NULL,
  `resource_id` VARCHAR(64) NOT NULL,
  `account_id`  BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `op_type`     VARCHAR(16) NOT NULL COMMENT 'CREATE/MODIFY/SUSPEND/RESUME/DELETE',
  `idempot_key` VARCHAR(64) NOT NULL COMMENT '通常为 order_id;控制器据此去重',
  `params_json` JSON NOT NULL COMMENT '下发给 rc-* 的 ResourceSpec',
  `status`      VARCHAR(16) NOT NULL DEFAULT 'INIT' COMMENT 'INIT/DISPATCHED/DONE/FAILED/RETRYING',
  `retry_count` INT NOT NULL DEFAULT 0,
  `last_error`  VARCHAR(512) DEFAULT NULL,
  `dispatched_at` DATETIME DEFAULT NULL,
  `finished_at` DATETIME DEFAULT NULL,
  `created_at`  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`task_id`),
  UNIQUE KEY `uk_idem` (`idempot_key`,`op_type`) COMMENT '同一订单同一操作只下发一次',
  KEY `idx_resource` (`resource_id`),
  KEY `idx_status_updated` (`status`,`updated_at`) COMMENT '卡单扫描与重试'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='履约任务;gRPC 同步下发,Kafka 仅作异步回调与重试通道(裁决S21)';

-- -----------------------------------------------------------------------------
-- flow_definition / flow_instance / step_instance — 轻量工作流引擎 (03§4.3.3)
--
-- Self-built deliberately: 03§4.3.3 forbids introducing Temporal/Camunda.
-- The platform needs step sequencing with compensation, not a BPMN engine, and
-- a heavyweight workflow product is one more stateful system for a six-person
-- SRE team to operate.
-- -----------------------------------------------------------------------------
CREATE TABLE `flow_definition` (
  `def_key`     VARCHAR(64) NOT NULL COMMENT 'fulfill-instance / resize-flow / renew-flow / release-flow',
  `def_version` INT NOT NULL DEFAULT 1,
  `steps_json`  JSON NOT NULL COMMENT '步骤 DAG:每步含正向操作与补偿操作(03§8.4)',
  `status`      TINYINT NOT NULL DEFAULT 1 COMMENT '1启用 2停用',
  `created_at`  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`def_key`,`def_version`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='流程定义;不引入候选池外的重型工作流组件';

CREATE TABLE `flow_instance` (
  `flow_instance_id` BIGINT UNSIGNED NOT NULL,
  `def_key`          VARCHAR(64) NOT NULL,
  `def_version`      INT NOT NULL DEFAULT 1,
  `account_id`       BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `biz_key`          VARCHAR(64) NOT NULL COMMENT 'order_id / resource_id',
  `status`           VARCHAR(16) NOT NULL DEFAULT 'RUNNING' COMMENT 'RUNNING/COMPLETED/COMPENSATING/COMPENSATED/FAILED',
  `payload_json`     JSON NOT NULL,
  `current_step`     INT NOT NULL DEFAULT 0,
  `next_fire_at`     DATETIME DEFAULT NULL COMMENT '定时器;扫描此列 + 分布式锁驱动,不依赖额外调度组件',
  `version`          INT NOT NULL DEFAULT 0,
  `created_at`       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`flow_instance_id`),
  UNIQUE KEY `uk_biz` (`def_key`,`biz_key`) COMMENT '同一业务键同一流程只跑一次',
  KEY `idx_next_fire` (`next_fire_at`,`status`) COMMENT '定时器扫描索引',
  KEY `idx_account` (`account_id`,`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='流程实例';

CREATE TABLE `step_instance` (
  `step_id`          BIGINT UNSIGNED NOT NULL,
  `flow_instance_id` BIGINT UNSIGNED NOT NULL,
  `account_id`       BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键',
  `step_index`       INT NOT NULL,
  `step_type`        VARCHAR(64) NOT NULL COMMENT 'check_quota/create_resource/start_metering/confirm_order',
  `status`           VARCHAR(16) NOT NULL DEFAULT 'PENDING' COMMENT 'PENDING/RUNNING/DONE/FAILED/COMPENSATED',
  `attempt`          INT NOT NULL DEFAULT 0,
  `input_json`       JSON DEFAULT NULL,
  `output_json`      JSON DEFAULT NULL,
  `error_msg`        VARCHAR(512) DEFAULT NULL,
  `started_at`       DATETIME DEFAULT NULL,
  `finished_at`      DATETIME DEFAULT NULL,
  PRIMARY KEY (`step_id`),
  UNIQUE KEY `uk_flow_step` (`flow_instance_id`,`step_index`),
  KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='步骤实例;引擎 at-least-once 投递,执行器必须幂等';

-- -----------------------------------------------------------------------------
-- outbox_message — 本地消息表 (03§8.2)
-- Written in the SAME transaction as the resource_instance row update.
-- -----------------------------------------------------------------------------
CREATE TABLE `outbox_message` (
  `id`            BIGINT UNSIGNED NOT NULL,
  `account_id`    BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键,与业务表同片同事务',
  `biz_type`      VARCHAR(32) NOT NULL COMMENT 'resource/flow',
  `biz_key`       VARCHAR(64) NOT NULL,
  `topic`         VARCHAR(64) NOT NULL COMMENT 'cloud.resource.lifecycle.event 等',
  `partition_key` VARCHAR(64) NOT NULL COMMENT '资源事件按 resource_id(04§5.4)',
  `event_id`      VARCHAR(64) NOT NULL,
  `payload`       MEDIUMTEXT NOT NULL,
  `status`        TINYINT NOT NULL DEFAULT 0 COMMENT '0待发 1已发 2放弃',
  `retry_count`   INT NOT NULL DEFAULT 0,
  `next_retry`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `created_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_event_id` (`event_id`),
  KEY `idx_status_retry` (`status`,`next_retry`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='本地消息表';

-- -----------------------------------------------------------------------------
-- 对账视图:卡在中间态的资源
--
-- 03§8.5 编排↔控制器对账. A resource stuck in an intermediate state past its
-- timeout is either a lost callback or a genuinely failed operation. Either way
-- it holds quota and may be billing, so it must be found and resolved rather
-- than left to sit.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE VIEW `v_resource_stuck_intermediate` AS
SELECT
  r.resource_id,
  r.account_id,
  r.product_code,
  r.status,
  r.updated_at,
  TIMESTAMPDIFF(MINUTE, r.updated_at, UTC_TIMESTAMP()) AS stuck_minutes,
  CASE r.status
    WHEN 'CREATING'  THEN 15
    WHEN 'UPGRADING' THEN 30
    WHEN 'RELEASING' THEN 30
  END AS timeout_minutes
FROM resource_instance r
WHERE r.status IN ('CREATING','UPGRADING','RELEASING')
  AND TIMESTAMPDIFF(MINUTE, r.updated_at, UTC_TIMESTAMP()) >
      CASE r.status
        WHEN 'CREATING'  THEN 15
        WHEN 'UPGRADING' THEN 30
        WHEN 'RELEASING' THEN 30
      END;

-- -----------------------------------------------------------------------------
-- 对账视图:计费状态与资源状态不一致
--
-- A resource that is RUNNING but has no billing_start is being served for free;
-- one that is RELEASED but still carries a billing_start with no released_at
-- may still be metered. Both are money-visible and must close same-day.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE VIEW `v_resource_billing_exceptions` AS
SELECT
  r.resource_id,
  r.account_id,
  r.product_code,
  r.status,
  r.billing_start,
  r.released_at,
  CASE
    WHEN r.status IN ('RUNNING','UPGRADING') AND r.billing_start IS NULL THEN 'RUNNING_NOT_BILLED'
    WHEN r.status = 'RELEASED' AND r.released_at IS NULL THEN 'RELEASED_NO_TIMESTAMP'
    WHEN r.status = 'LOCKED' AND r.locked_at IS NULL THEN 'LOCKED_NO_TIMESTAMP'
    ELSE 'OK'
  END AS exception_type
FROM resource_instance r
WHERE (r.status IN ('RUNNING','UPGRADING') AND r.billing_start IS NULL)
   OR (r.status = 'RELEASED' AND r.released_at IS NULL)
   OR (r.status = 'LOCKED' AND r.locked_at IS NULL);
