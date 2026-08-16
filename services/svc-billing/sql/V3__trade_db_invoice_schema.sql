-- =============================================================================
-- svc-billing DDL — trade_db (发票与红冲, 二期 M-4.3)
--
-- 决策 D6/B6:发票是税务文档,已开具不可删,红冲=开一张全额负数发票冲抵(中国税务
-- 红字发票语义).原票置 VOIDED 但记录保留,与审计链同构(终态不可逆).
-- 一期 event.BillingInvoiceEvent topic 已预留(event.go:77),本 DDL 补发票文档表.
--
-- Sharding: account_id 单键, trade_db 8库×16表 (04§6.3). 冗余分片键禁止跨库 JOIN.
-- 金额一律 DECIMAL 禁止浮点 (03§6).
--
-- Source of truth: 01-product-catalog.md §12.3; 03-backend-services.md §4.2.4;
-- 09-roadmap.md §4.3 M-4.3, §4.4 B6.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- invoice — 发票文档
--
-- One row per issued tax document. uk_invoice makes the issue idempotent. A
-- 红冲 (red-credit) reversal does NOT touch the original row's amount — it sets
-- status=VOIDED and voided_by_id pointing to the negative reversal invoice,
-- retaining the original for audit (terminal-irreversible, like the audit
-- chain). amount on a reversal is negative; on a normal invoice, non-negative.
--
-- Money invariants enforced in the schema:
--   * A normal (DRAFT/ISSUED) invoice amount >= 0; a reversal's amount < 0.
--   * status transitions: DRAFT→ISSUED→VOIDED; VOIDED is terminal.
--   * voided_by_id is set iff status=VOIDED; reverses_id set iff this row is
--     itself a reversal.
-- -----------------------------------------------------------------------------
CREATE TABLE `invoice` (
  `invoice_id`    VARCHAR(64) NOT NULL COMMENT '发票ID;全局唯一',
  `account_id`    BIGINT UNSIGNED NOT NULL COMMENT '分片键(≡uid≡user_id≡tenant_id)',
  `bill_period`   CHAR(7) NOT NULL COMMENT '账期 YYYY-MM',
  `title`         VARCHAR(256) NOT NULL COMMENT '发票抬头',
  `tax_no`        VARCHAR(64) NOT NULL DEFAULT '' COMMENT '纳税人识别号',
  `amount`        DECIMAL(12,2) NOT NULL COMMENT '发票总额;正常票≥0,红冲票<0(全额负数)',
  `status`        TINYINT NOT NULL DEFAULT 1 COMMENT '1DRAFT 2ISSUED 3VOIDED;VOIDED终态',
  `issued_at`     DATETIME DEFAULT NULL COMMENT '开具时间;DRAFT为空',
  `voided_at`     DATETIME DEFAULT NULL COMMENT '红冲时间;非VOIDED为空',
  `voided_by_id`  VARCHAR(64) NOT NULL DEFAULT '' COMMENT '红冲票ID;仅VOIDED行非空',
  `reverses_id`   VARCHAR(64) NOT NULL DEFAULT '' COMMENT '本票红冲的原票ID;仅红冲票非空',
  `created_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`invoice_id`),
  UNIQUE KEY `uk_invoice` (`account_id`,`invoice_id`) COMMENT '幂等;同ID只一行',
  KEY `idx_acc_period` (`account_id`,`bill_period`,`status`) COMMENT '按账期查发票'
) ENGINE=InnoDB DEFAULT CHARSET=utf2mb4
  COMMENT='发票文档;红冲不删原票,置VOIDED并开负数票,终态不可逆(B6/M-4.3)';

-- -----------------------------------------------------------------------------
-- invoice_item — 发票明细行
--
-- One row per line item. A 红冲 invoice carries the negated descriptions and
-- amounts of its original. amount on a reversal line is negative.
-- -----------------------------------------------------------------------------
CREATE TABLE `invoice_item` (
  `id`          BIGINT UNSIGNED NOT NULL,
  `invoice_id`  VARCHAR(64) NOT NULL,
  `account_id`  BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键;禁止跨库 JOIN',
  `description` VARCHAR(256) NOT NULL COMMENT '明细描述;红冲行前缀"红冲:"',
  `amount`      DECIMAL(12,2) NOT NULL COMMENT '明细金额;红冲行为负',
  `seq`         INT NOT NULL COMMENT '行序号',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_invoice_seq` (`account_id`,`invoice_id`,`seq`) COMMENT '一行一序号',
  KEY `idx_invoice` (`account_id`,`invoice_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf2mb4
  COMMENT='发票明细行;红冲行金额为负(B6/M-4.3)';
