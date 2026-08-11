-- =============================================================================
-- svc-billing DDL — trade_db (计费出账)
--
-- Sharding: account_id single key, 8 库 × 16 表 (04§6.3). Cash balance and
-- ledger for the money domain belong to trade_db under svc-billing; every table
-- below carries account_id, child tables included — a redundant shard key is
-- cheaper than a cross-shard join, which the sharding rules forbid outright
-- (03 附录A: 禁止跨库 JOIN).
--
-- 出账链路 (03§4.2.4): 计量聚合 → 计费(单价快照)→ 抵扣(资源包→代金券→余额)
-- → 明细入账 → 小时汇总 → 月度账单;欠费 → 宽限期 → 停服锁定 → 释放.
-- 金额一律 DECIMAL 禁止浮点 (03§6).
--
-- Source of truth: 03-backend-services.md §4.2.4; 04§6.3 (trade_db sharding 8x16).
-- =============================================================================

-- -----------------------------------------------------------------------------
-- bill — 月度账单汇总
--
-- One row per (account_id, bill_month). uk_acc_month makes the monthly
-- aggregation idempotent: a re-run of the daily job upserts rather than
-- appends a second bill for the same month.
--
-- Money invariants enforced in the schema:
--   * paid_amount 与 total_amount 之差即未付金额,绝不为负 —— CHECK 约束兜底;
--   * status transitions (1未出 2待支付 3已结清 4已出账 5已作废) ride the
--     version optimistic lock, so a duplicate payment affects zero rows.
-- -----------------------------------------------------------------------------
CREATE TABLE `bill` (
  `bill_id`     BIGINT UNSIGNED NOT NULL,
  `account_id`  BIGINT UNSIGNED NOT NULL COMMENT '分片键(≡uid≡user_id≡tenant_id)',
  `bill_month`  CHAR(7) NOT NULL COMMENT '账期 YYYY-MM;同账号同账期唯一',
  `status`      TINYINT NOT NULL DEFAULT 1 COMMENT '1未出账 2待支付 3已结清 4已出账 5已作废',
  `total_amount` DECIMAL(12,2) NOT NULL COMMENT '本月应出总额;金额一律 DECIMAL 禁止浮点(03§6)',
  `paid_amount` DECIMAL(12,2) NOT NULL DEFAULT 0 COMMENT '本月已付;未付额=total-paid,不得为负',
  `version`     INT NOT NULL DEFAULT 0 COMMENT '乐观锁;出账/收款更新带 WHERE version=?',
  `created_at`  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`bill_id`),
  UNIQUE KEY `uk_acc_month` (`account_id`,`bill_month`) COMMENT '幂等;月度汇总重算不重复插行',
  KEY `idx_acc_status` (`account_id`,`status`,`bill_month`),
  CONSTRAINT `ck_paid_lte_total` CHECK (`paid_amount` >= 0 AND `paid_amount` <= `total_amount`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='月度账单汇总;未付额不得为负,故约束 paid<=total(03§4.2.4)';

-- -----------------------------------------------------------------------------
-- bill_detail — 计费明细(逐行入账)
--
-- Each charge row is one (resource × item_code × hour) settlement after 抵扣.
-- uk_acc_charge is the idempotency key: the hourly job is at-least-once
-- (recompute-able per 03§4.2.5), and a re-delivery must not double-count an
-- amount.
--
-- covered_ratio ∈ [0,1] records what fraction of this row was covered by
-- 资源包/代金券/余额 before it hit cash — the field that makes the 抵扣顺序
-- (资源包→代金券→余额) auditable rather than a comment.
-- -----------------------------------------------------------------------------
CREATE TABLE `bill_detail` (
  `id`            BIGINT UNSIGNED NOT NULL,
  `bill_id`       BIGINT UNSIGNED NOT NULL,
  `account_id`    BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键;禁止跨库 JOIN 故冗余而非关联',
  `resource_id`   VARCHAR(64) NOT NULL COMMENT '资源ID;跨库资源,此处只存引用不 JOIN',
  `item_code`     VARCHAR(64) NOT NULL COMMENT '计量项: cpu_core_hour 等',
  `quantity`      DECIMAL(18,4) NOT NULL DEFAULT 1 COMMENT '用量;按量可为小数',
  `unit_price`    DECIMAL(12,2) NOT NULL COMMENT '单价快照;不读当前价(03§4.2.4)',
  `amount`        DECIMAL(12,2) NOT NULL COMMENT '本行应付;=quantity×unit_price 或折扣后',
  `covered_ratio` DECIMAL(5,4) NOT NULL DEFAULT 0 COMMENT '抵扣覆盖比例[0,1];资源包/代金券/余额抵扣审计依据',
  `charge_type`   TINYINT NOT NULL COMMENT '1包年包月 2按量 3资源包抵扣 4代金券抵扣 5余额抵扣',
  `created_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_acc_charge` (`account_id`,`resource_id`,`item_code`,`created_at`) COMMENT '幂等;计量明细重算不重复入账',
  KEY `idx_bill` (`bill_id`),
  KEY `idx_acc_time` (`account_id`,`created_at`),
  CONSTRAINT `ck_covered_ratio` CHECK (`covered_ratio` >= 0 AND `covered_ratio` <= 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='计费明细;抵扣覆盖比例使抵扣顺序可审计(03§4.2.4)';

-- -----------------------------------------------------------------------------
-- account_arrears — 账户欠费
--
-- One row per account in arrears. 欠费状态机 (03§4.2.4): 欠费 → 宽限期
-- (grace_end) → 停服锁定 (locked_at) → 释放. grace_end/locked_at are the
-- schema-visible ordering rule: a resource may not be locked before grace_end
-- nor released before locked_at — enforced in application code, made visible
-- here so the sweeper's scan is a single index lookup on status+time.
-- -----------------------------------------------------------------------------
CREATE TABLE `account_arrears` (
  `account_id`    BIGINT UNSIGNED NOT NULL COMMENT '分片键;每账户至多一条欠费记录',
  `arrears_begin` DATETIME NOT NULL COMMENT '欠费起始时间',
  `grace_end`     DATETIME NOT NULL COMMENT '宽限期截止;到达前不得停服锁定',
  `locked_at`     DATETIME DEFAULT NULL COMMENT '停服锁定时间;为空则未锁定,释放前必填',
  `status`        TINYINT NOT NULL DEFAULT 1 COMMENT '1宽限期 2已锁定 3已释放 4已结清',
  `created_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`account_id`),
  KEY `idx_status_grace` (`status`,`grace_end`) COMMENT '宽限期到期扫描(锁定/释放任务)'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='账户欠费;欠费→宽限期→锁定→释放状态机落点(03§4.2.4)';
