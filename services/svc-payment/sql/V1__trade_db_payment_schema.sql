-- =============================================================================
-- svc-payment DDL — trade_db (支付与账本)
--
-- Sharding: account_id single key, 8 库 × 16 表 (04§6.3).
--
-- Adjudication S29: cash balance and its journal live HERE, in the trade
-- domain, not on the account table. 03§6.1's account table keeps identity
-- attributes only. Identity is read on every gateway request; balance is
-- written on every charge — sharing a row would put a financial write path
-- behind the hottest read path on the platform.
--
-- Phase-1 scope (03§4.2.3): 余额支付 + 内部模拟渠道. Third-party channel
-- integration (支付宝/微信/银联) is phase 2; the schema carries the channel
-- columns from Day 1 so adding a real channel is configuration, not migration
-- (架构原则 12 模型先行).
--
-- Source of truth: 03-backend-services.md §4.2.3/§8.3, 01§12.3 抵扣顺序.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- account_balance — 现金余额
--
-- available + frozen is the customer's total holding. Freezing reserves funds
-- for an in-flight order without removing them from the account, which is what
-- lets an order hold its price while provisioning runs.
--
-- available is UNSIGNED-by-constraint (CHECK): overdraft is an arrears state on
-- the account (03§5.4 欠费生命周期), never a negative number here. A negative
-- balance would silently extend credit that no policy authorised.
-- -----------------------------------------------------------------------------
CREATE TABLE `account_balance` (
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '分片键(≡uid≡user_id≡tenant_id)',
  `available`  DECIMAL(14,6) NOT NULL DEFAULT 0 COMMENT '可用余额;绝不为负,欠费走账号状态机而非负余额',
  `frozen`     DECIMAL(14,6) NOT NULL DEFAULT 0 COMMENT '冻结金额;履约中订单的预留',
  `currency`   CHAR(3) NOT NULL DEFAULT 'CNY' COMMENT '多币种 Day1 预留(架构原则12)',
  `version`    INT NOT NULL DEFAULT 0 COMMENT '乐观锁;扣减带 WHERE version=? 防并发双花',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`account_id`),
  CONSTRAINT `ck_available_non_negative` CHECK (`available` >= 0),
  CONSTRAINT `ck_frozen_non_negative` CHECK (`frozen` >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='现金余额;归 trade_db 而非 account 表(裁决S29)';

-- -----------------------------------------------------------------------------
-- ledger_entry — 余额流水(只增不改)
--
-- Append-only (00§3.3 余额流水只增不改). Entries are never UPDATEd or DELETEd;
-- a correction is a new compensating entry beside the original.
--
-- balance_after is a redundant running total, and that redundancy is the point:
-- reading forward, each entry's balance_after must equal the previous plus this
-- entry's signed amount. A removed, reordered, or edited row breaks the chain
-- and is detectable without recomputing from the beginning of time. An edited
-- journal is otherwise indistinguishable from a fraudulent one.
--
-- amount is always POSITIVE; entry_type carries the direction. Signed amounts
-- invite a sign error to silently invert a charge into a credit.
-- -----------------------------------------------------------------------------
CREATE TABLE `ledger_entry` (
  `entry_id`        BIGINT UNSIGNED NOT NULL COMMENT '号段服务发号,内嵌分片因子(04§6.6)',
  `account_id`      BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `entry_type`      VARCHAR(16) NOT NULL COMMENT 'RECHARGE/CONSUME/REFUND/ADJUST_DEBIT/ADJUST_CREDIT/FREEZE/UNFREEZE',
  `amount`          DECIMAL(14,6) NOT NULL COMMENT '恒为正;方向由 entry_type 决定,不用符号表达',
  `balance_after`   DECIMAL(14,6) NOT NULL COMMENT '本笔之后的可用余额;链式自校验,断链即被篡改',
  `biz_type`        VARCHAR(32) NOT NULL COMMENT 'order/bill/refund/recharge/adjust',
  `biz_key`         VARCHAR(64) NOT NULL COMMENT '订单号/账单号/退款单号;账单行可追溯至成因',
  `idempotency_key` VARCHAR(64) NOT NULL COMMENT '幂等键;重复入账为无操作而非重复扣款',
  `remark`          VARCHAR(256) DEFAULT NULL COMMENT '对客账单展示文案',
  `occurred_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`entry_id`),
  UNIQUE KEY `uk_idempotency` (`account_id`,`idempotency_key`) COMMENT '幂等终裁',
  KEY `idx_account_time` (`account_id`,`occurred_at`) COMMENT '账单流水查询',
  KEY `idx_biz` (`biz_type`,`biz_key`) COMMENT '按订单反查流水(客诉排查)'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='余额流水;只增不改,SUM(signed amount) 必须复现 account_balance';

-- -----------------------------------------------------------------------------
-- payment_record — 支付单
--
-- Phase-1 channels: BALANCE (余额) and MOCK (内部模拟). The channel columns
-- exist now so onboarding 支付宝/微信/银联 in phase 2 needs no migration.
-- -----------------------------------------------------------------------------
CREATE TABLE `payment_record` (
  `payment_id`      BIGINT UNSIGNED NOT NULL,
  `account_id`      BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `order_id`        BIGINT UNSIGNED NOT NULL,
  `channel`         VARCHAR(16) NOT NULL COMMENT '一期 BALANCE/MOCK;二期 ALIPAY/WECHAT/UNIONPAY',
  `channel_txn_no`  VARCHAR(64) DEFAULT NULL COMMENT '渠道流水号;渠道回调时回填',
  `amount`          DECIMAL(12,2) NOT NULL COMMENT '支付金额;与订单 payable_amount 必须一致',
  `balance_paid`    DECIMAL(12,2) NOT NULL DEFAULT 0 COMMENT '余额支付部分',
  `coupon_paid`     DECIMAL(12,2) NOT NULL DEFAULT 0 COMMENT '代金券抵扣部分',
  `channel_paid`    DECIMAL(12,2) NOT NULL DEFAULT 0 COMMENT '三方渠道支付部分',
  `status`          TINYINT NOT NULL DEFAULT 1 COMMENT '1待支付 2支付中 3已支付 4支付失败 5已关闭',
  `client_token`    VARCHAR(64) NOT NULL COMMENT '幂等键',
  `version`         INT NOT NULL DEFAULT 0,
  `paid_at`         DATETIME DEFAULT NULL,
  `expire_at`       DATETIME NOT NULL COMMENT '支付超时;超时自动关单并解冻',
  `created_at`      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`payment_id`),
  UNIQUE KEY `uk_client_token` (`account_id`,`client_token`),
  UNIQUE KEY `uk_order` (`order_id`) COMMENT '一单一支付单,防止同一订单重复支付',
  KEY `idx_status_expire` (`status`,`expire_at`) COMMENT '超时关单扫描',
  KEY `idx_channel_txn` (`channel`,`channel_txn_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='支付单;抵扣顺序 资源包→代金券→余额→三方渠道(01§12.3)';

-- -----------------------------------------------------------------------------
-- payment_callback_log — 渠道回调流水
--
-- The unique key on (channel, channel_txn_no) is what makes repeated channel
-- notifications safe (03§8.3). Payment channels retry aggressively and do not
-- guarantee exactly-once delivery; without this key, a retried notification
-- settles the same payment twice and the customer is credited twice.
--
-- Callbacks are logged BEFORE they drive any state change, so a callback that
-- crashes mid-processing is still visible for investigation rather than lost.
-- -----------------------------------------------------------------------------
CREATE TABLE `payment_callback_log` (
  `id`             BIGINT UNSIGNED NOT NULL,
  `account_id`     BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键;解析回调后回填',
  `channel`        VARCHAR(16) NOT NULL,
  `channel_txn_no` VARCHAR(64) NOT NULL COMMENT '渠道流水号',
  `payment_id`     BIGINT UNSIGNED DEFAULT NULL COMMENT '匹配到的支付单;匹配失败为空,挂起人工',
  `raw_payload`    TEXT NOT NULL COMMENT '原始报文;验签与争议举证依据',
  `signature_ok`   TINYINT NOT NULL COMMENT '0验签失败 1验签通过;失败不得驱动任何状态变更',
  `processed`      TINYINT NOT NULL DEFAULT 0 COMMENT '0待处理 1已处理 2重复(幂等命中) 3失败挂起',
  `process_result` VARCHAR(256) DEFAULT NULL,
  `received_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_channel_txn` (`channel`,`channel_txn_no`) COMMENT '渠道重复通知防重复销账',
  KEY `idx_processed` (`processed`,`received_at`) COMMENT '待处理与挂起扫描'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='渠道回调流水;先落库再驱动状态,验签失败不驱动任何变更';

-- -----------------------------------------------------------------------------
-- recharge_record — 充值单
-- -----------------------------------------------------------------------------
CREATE TABLE `recharge_record` (
  `recharge_id`    BIGINT UNSIGNED NOT NULL,
  `account_id`     BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `amount`         DECIMAL(12,2) NOT NULL,
  `channel`        VARCHAR(16) NOT NULL,
  `channel_txn_no` VARCHAR(64) DEFAULT NULL,
  `status`         TINYINT NOT NULL DEFAULT 1 COMMENT '1待支付 2已到账 3失败 4已关闭',
  `client_token`   VARCHAR(64) NOT NULL COMMENT '幂等键',
  `ledger_entry_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '到账后的流水号;对账连接点',
  `version`        INT NOT NULL DEFAULT 0,
  `credited_at`    DATETIME DEFAULT NULL,
  `created_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`recharge_id`),
  UNIQUE KEY `uk_client_token` (`account_id`,`client_token`),
  KEY `idx_account_status` (`account_id`,`status`,`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='充值单';

-- -----------------------------------------------------------------------------
-- outbox_message — 本地消息表 (03§8.2)
-- Written in the SAME transaction as the balance/payment change.
--
-- 该表由 svc-order 的 V1 建立 —— trade_db 是 5 个服务共用的逻辑库,一张表只能
-- 有一个权威定义。这里曾经复制了一份,后果不是"多一层保险"而是迁移必挂:
-- 第二个目录执行到这条 CREATE TABLE 时已经存在,直接 Error 1050 中断整个迁移;
-- 而两份定义各自漂移时(这里的 biz_type/topic 注释与 order 侧不同),哪一份生效
-- 只取决于目录顺序,建出来的表与谁写的服务对不上。
--
-- 权威定义: services/svc-order/sql/V1__trade_db_order_schema.sql
-- 本服务同样写这张表(充值到账/支付成功事件),列与索引完全一致。
-- -----------------------------------------------------------------------------

-- -----------------------------------------------------------------------------
-- 对账视图:余额 vs 流水
--
-- 03§8.5 T+1 对账. Any account where SUM(signed ledger amount) does not equal
-- available + frozen is a hard reconciliation failure — the balance cannot be
-- explained by its own history. 09 A2 requires 无未解释差异, and this is the
-- most unexplainable difference there is.
--
-- Freeze/unfreeze move funds between available and frozen without leaving the
-- account, so the comparison is against available + frozen, not available
-- alone. Comparing against available alone would flag every account with an
-- in-flight order.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE VIEW `v_balance_reconcile_exceptions` AS
SELECT
  b.account_id,
  b.available,
  b.frozen,
  (b.available + b.frozen) AS total_held,
  COALESCE(j.journal_sum, 0) AS journal_sum,
  (COALESCE(j.journal_sum, 0) - (b.available + b.frozen)) AS difference,
  COALESCE(j.entry_count, 0) AS entry_count,
  b.updated_at
FROM account_balance b
LEFT JOIN (
  SELECT
    account_id,
    SUM(CASE
          WHEN entry_type IN ('RECHARGE','REFUND','ADJUST_CREDIT') THEN amount
          WHEN entry_type IN ('CONSUME','ADJUST_DEBIT')            THEN -amount
          ELSE 0
        END) AS journal_sum,
    COUNT(*) AS entry_count
  FROM ledger_entry
  GROUP BY account_id
) j ON j.account_id = b.account_id
WHERE COALESCE(j.journal_sum, 0) <> (b.available + b.frozen);

-- -----------------------------------------------------------------------------
-- 对账视图:支付单 vs 订单
--
-- Surfaces payments whose amount disagrees with the order, and callbacks that
-- arrived but never matched a payment record. Both are money-visible and must
-- be closed within the day (03§8.5).
-- -----------------------------------------------------------------------------
CREATE OR REPLACE VIEW `v_payment_reconcile_exceptions` AS
SELECT
  p.payment_id,
  p.account_id,
  p.order_id,
  p.channel,
  p.amount,
  (p.balance_paid + p.coupon_paid + p.channel_paid) AS component_sum,
  p.status,
  CASE
    WHEN (p.balance_paid + p.coupon_paid + p.channel_paid) <> p.amount THEN 'COMPONENT_MISMATCH'
    WHEN p.status = 2 AND p.updated_at < DATE_SUB(UTC_TIMESTAMP(), INTERVAL 30 MINUTE) THEN 'PAYING_STUCK'
    WHEN p.status = 1 AND p.expire_at < UTC_TIMESTAMP() THEN 'EXPIRED_NOT_CLOSED'
    ELSE 'OK'
  END AS exception_type,
  p.updated_at
FROM payment_record p
WHERE
  (p.balance_paid + p.coupon_paid + p.channel_paid) <> p.amount
  OR (p.status = 2 AND p.updated_at < DATE_SUB(UTC_TIMESTAMP(), INTERVAL 30 MINUTE))
  OR (p.status = 1 AND p.expire_at < UTC_TIMESTAMP());
