-- =============================================================================
-- svc-order DDL — trade_db (订单中心)
--
-- Sharding: account_id single key, 8 库 × 16 表 (04§6.3). Every table below
-- carries account_id, including child tables where it is redundant — a
-- redundant shard key is cheaper than a cross-shard join, which the sharding
-- rules forbid outright (03 附录A: 禁止跨库 JOIN).
--
-- 对标启示 9: 订单中心是商业化复杂度的中枢. One unified model covers all five
-- transaction types; product lines must not build their own order tables.
--
-- Source of truth: 03-backend-services.md §4.2.2 and §6.3, 01§5.2.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- order_main — 订单主表
--
-- Two invariants this schema enforces:
--
--   1. uk_client_token makes retries safe. The gateway screens duplicates with
--      Redis SETNX, but Redis is a cache: this unique index is the final
--      arbiter (03§8.3). Without it, a network retry on 下单 double-charges.
--
--   2. version drives the optimistic lock. Every state transition runs
--      UPDATE ... WHERE status = ? AND version = ?, so a duplicate payment
--      callback affects zero rows and is idempotent by construction rather
--      than by the caller remembering to check.
-- -----------------------------------------------------------------------------
CREATE TABLE `order_main` (
  `order_id`        BIGINT UNSIGNED NOT NULL COMMENT '订单ID;号段服务发号,内嵌分片因子(04§6.6)',
  `order_no`        VARCHAR(32) NOT NULL COMMENT '对外单号,客服与用户可见',
  `account_id`      BIGINT UNSIGNED NOT NULL COMMENT '分片键(≡uid≡user_id≡tenant_id)',
  `order_type`      TINYINT NOT NULL COMMENT '1新购 2续费 3升配 4降配 5退订;五类交易统一模型(对标启示9)',
  `product_code`    VARCHAR(32) NOT NULL,
  `charge_type`     TINYINT NOT NULL COMMENT '1包年包月 2按量;3资源包/4抢占式 模型预留一期不售(D6)',
  `snapshot_id`     VARCHAR(32) NOT NULL COMMENT '价格快照ID;出账/对账/退订折算一律以快照为准,不读当前价',
  `original_amount` DECIMAL(12,2) NOT NULL COMMENT '目录价小计',
  `discount_amount` DECIMAL(12,2) NOT NULL DEFAULT 0 COMMENT '促销+代金券抵扣合计',
  `payable_amount`  DECIMAL(12,2) NOT NULL COMMENT '应付;金额一律 DECIMAL 禁止浮点(03§6)',
  `paid_amount`     DECIMAL(12,2) NOT NULL DEFAULT 0 COMMENT '实付;与 payable 不符即对账差异,不得静默接受',
  `status`          TINYINT NOT NULL COMMENT '1待支付 2已支付 3履约中 4已完成 5已取消 6退款中 7已退款',
  `resource_id`     VARCHAR(64) DEFAULT NULL COMMENT '新购在履约回调后回填;其余类型下单即必填',
  `client_token`    VARCHAR(64) NOT NULL COMMENT '幂等键(UUID,10分钟窗口)',
  `version`         INT NOT NULL DEFAULT 0 COMMENT '乐观锁;状态迁移带 WHERE status=? AND version=?',
  `paid_at`         DATETIME DEFAULT NULL,
  `created_at`      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`order_id`),
  UNIQUE KEY `uk_order_no` (`order_no`),
  UNIQUE KEY `uk_client_token` (`account_id`,`client_token`) COMMENT '幂等终裁;重试不会重复下单',
  KEY `idx_acc_status` (`account_id`,`status`,`created_at`),
  KEY `idx_resource` (`resource_id`) COMMENT '按资源反查订单(续费/退订链路)',
  KEY `idx_status_created` (`status`,`created_at`) COMMENT '待支付超时扫描'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='订单主表;paid→fulfilling 是唯一触发资源创建的迁移(03§4.2.2)';

-- -----------------------------------------------------------------------------
-- order_item — 订单行
-- -----------------------------------------------------------------------------
CREATE TABLE `order_item` (
  `id`             BIGINT UNSIGNED NOT NULL,
  `order_id`       BIGINT UNSIGNED NOT NULL,
  `account_id`     BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键;禁止跨库 JOIN 故冗余而非关联',
  `resource_id`    VARCHAR(64) DEFAULT NULL COMMENT '续费/变配时非空',
  `sku_code`       VARCHAR(64) NOT NULL,
  `region_id`      VARCHAR(32) NOT NULL COMMENT '资源模型 Day1 带 region(对标启示6)',
  `zone_id`        VARCHAR(32) DEFAULT NULL,
  `quantity`       INT NOT NULL DEFAULT 1,
  `duration`       INT DEFAULT NULL COMMENT '时长;按量为 NULL',
  `duration_unit`  VARCHAR(8) DEFAULT NULL COMMENT 'MONTH/YEAR/HOUR',
  `snapshot_id`    VARCHAR(32) NOT NULL COMMENT '本行的价格快照;对账依据',
  `item_amount`    DECIMAL(12,2) NOT NULL,
  `created_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_order` (`order_id`),
  KEY `idx_account` (`account_id`,`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='订单行';

-- -----------------------------------------------------------------------------
-- order_state_log — 状态迁移流水
--
-- Append-only. Every transition writes a row, so "why is this order in this
-- state" is answerable months later without replaying Kafka. This is the
-- order-domain counterpart to the audit chain: 09 A2 requires bills to have no
-- unexplained differences, and an unexplained state is an unexplained bill.
-- -----------------------------------------------------------------------------
CREATE TABLE `order_state_log` (
  `id`          BIGINT UNSIGNED NOT NULL,
  `order_id`    BIGINT UNSIGNED NOT NULL,
  `account_id`  BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键',
  `from_status` TINYINT DEFAULT NULL COMMENT 'NULL 表示创建',
  `to_status`   TINYINT NOT NULL,
  `operator`    VARCHAR(64) NOT NULL COMMENT 'system/用户ID/运营ID',
  `reason`      VARCHAR(256) DEFAULT NULL COMMENT '失败原因、人工干预说明',
  `occurred_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_order_time` (`order_id`,`occurred_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='订单状态迁移流水;只增不改';

-- -----------------------------------------------------------------------------
-- refund_record — 退款单
--
-- Refunds run AFTER the resource is released (03§8.4 钱货两清有先后).
-- resource_released_at is NOT NULL before refund_status may leave PENDING:
-- the column exists so the ordering rule is visible in the schema, not only in
-- application code.
-- -----------------------------------------------------------------------------
CREATE TABLE `refund_record` (
  `refund_id`            BIGINT UNSIGNED NOT NULL,
  `order_id`             BIGINT UNSIGNED NOT NULL,
  `account_id`           BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `resource_id`          VARCHAR(64) DEFAULT NULL,
  `snapshot_id`          VARCHAR(32) NOT NULL COMMENT '折算基准取自快照,不读当前价',
  `total_periods`        INT DEFAULT NULL COMMENT '购买时长(周期数)',
  `used_periods`         INT DEFAULT NULL COMMENT '已用周期;不足一周期按一周期计',
  `refund_amount`        DECIMAL(12,2) NOT NULL COMMENT '按剩余时长折算;不足则为 0,绝不为负',
  `penalty_amount`       DECIMAL(12,2) NOT NULL DEFAULT 0 COMMENT '违约金',
  `resource_released_at` DATETIME DEFAULT NULL COMMENT '资源释放确认时间;为空不得放款',
  `refund_status`        TINYINT NOT NULL DEFAULT 1 COMMENT '1待处理 2资源已释放 3退款中 4已退款 5失败',
  `client_token`         VARCHAR(64) NOT NULL COMMENT '幂等键',
  `version`              INT NOT NULL DEFAULT 0,
  `created_at`           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`refund_id`),
  UNIQUE KEY `uk_client_token` (`account_id`,`client_token`),
  KEY `idx_order` (`order_id`),
  KEY `idx_status` (`refund_status`,`created_at`) COMMENT '待放款扫描'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='退款单;资源释放确认后方可放款(03§8.4)';

-- -----------------------------------------------------------------------------
-- outbox_message — 本地消息表 (03§8.2)
--
-- Written in the SAME transaction as the order row. A relay scans and publishes
-- to Kafka; business code is forbidden from producing money/resource events
-- directly, because a direct send can succeed while the transaction rolls back
-- (or vice versa) and the two states diverge with no way to reconcile.
-- -----------------------------------------------------------------------------
CREATE TABLE `outbox_message` (
  `id`            BIGINT UNSIGNED NOT NULL,
  `account_id`    BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键,与业务表同片同事务',
  `biz_type`      VARCHAR(32) NOT NULL COMMENT 'order/refund',
  `biz_key`       VARCHAR(64) NOT NULL COMMENT 'order_id / refund_id',
  `topic`         VARCHAR(64) NOT NULL COMMENT 'cloud.trade.order.event 等;topic 名不含环境(C2/S8)',
  `partition_key` VARCHAR(64) NOT NULL COMMENT '分区键;订单事件按 order_id(04§5.4)',
  `event_id`      VARCHAR(64) NOT NULL COMMENT '事件唯一ID;消费端据此幂等',
  `payload`       MEDIUMTEXT NOT NULL COMMENT '统一信封 {event_id,event_type,occurred_at,aggregate_id,payload}',
  `status`        TINYINT NOT NULL DEFAULT 0 COMMENT '0待发 1已发 2放弃(超阈值告警人工介入)',
  `retry_count`   INT NOT NULL DEFAULT 0,
  `next_retry`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '指数退避',
  `created_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_event_id` (`event_id`) COMMENT '防止同一事件重复入表',
  KEY `idx_status_retry` (`status`,`next_retry`) COMMENT 'Relay 扫描索引'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='本地消息表;资金/资源事件禁止业务代码直接发 Kafka(03§8.2)';

-- -----------------------------------------------------------------------------
-- idempotent_record — 幂等表 (03§8.3)
-- -----------------------------------------------------------------------------
CREATE TABLE `idempotent_record` (
  `id`         BIGINT UNSIGNED NOT NULL,
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键',
  `biz_type`   VARCHAR(32) NOT NULL,
  `biz_key`    VARCHAR(64) NOT NULL COMMENT 'event_id / task_id / ClientToken',
  `result`     TEXT DEFAULT NULL COMMENT '已有结果快照;重复请求直接返回,不重放副作用',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_biz` (`biz_type`,`biz_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='幂等记录';

-- -----------------------------------------------------------------------------
-- 对账视图:订单 vs 支付
--
-- T+1 reconciliation surface (03§8.5). Rows here are orders whose paid_amount
-- disagrees with payable_amount, or which have been stuck mid-flight past a
-- reasonable window. 09 A2 requires 无未解释差异 — this view is where the
-- daily job looks first.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE VIEW `v_order_reconcile_exceptions` AS
SELECT
  o.order_id,
  o.order_no,
  o.account_id,
  o.status,
  o.payable_amount,
  o.paid_amount,
  (o.paid_amount - o.payable_amount) AS amount_diff,
  o.created_at,
  o.updated_at,
  CASE
    WHEN o.status = 2 AND o.paid_amount <> o.payable_amount THEN 'AMOUNT_MISMATCH'
    WHEN o.status = 1 AND o.created_at < DATE_SUB(UTC_TIMESTAMP(), INTERVAL 30 MINUTE) THEN 'PAYMENT_TIMEOUT'
    WHEN o.status = 3 AND o.updated_at < DATE_SUB(UTC_TIMESTAMP(), INTERVAL 1 HOUR) THEN 'FULFILMENT_STUCK'
    WHEN o.status = 6 AND o.updated_at < DATE_SUB(UTC_TIMESTAMP(), INTERVAL 24 HOUR) THEN 'REFUND_STUCK'
    ELSE 'OK'
  END AS exception_type
FROM order_main o
WHERE
  (o.status = 2 AND o.paid_amount <> o.payable_amount)
  OR (o.status = 1 AND o.created_at < DATE_SUB(UTC_TIMESTAMP(), INTERVAL 30 MINUTE))
  OR (o.status = 3 AND o.updated_at < DATE_SUB(UTC_TIMESTAMP(), INTERVAL 1 HOUR))
  OR (o.status = 6 AND o.updated_at < DATE_SUB(UTC_TIMESTAMP(), INTERVAL 24 HOUR));
