-- =============================================================================
-- svc-billing DDL — trade_db (出账结算记录 + 账户结算锁)
--
-- 出账 (/internal/settle) 的已结算明细、逐 ChargeID 的结算计划(资源包消耗意图/
-- 实耗、现金扣款、券抵扣)以及幂等重放结果此前只在进程内存里:重启或第二个副本
-- 会对重试的 ChargeID 再扣一次,/internal/bills 与成本分析在重启后归零.
--
-- 为什么不复用 V1 的 bill_detail:
--   * 金额列 DECIMAL(12,2)/用量 DECIMAL(18,4) 存不下 pricing.Amount/metering.Quantity
--     的 6 位小数(micro-units),落库即舍入 —— 账单与现金流水对不上;
--   * 幂等键 uk_acc_charge 是 (account,resource,item,created_at),没有 charge_id,
--     无法按 ChargeID 重放;也没有结算计划(资源包意图/实耗)的落点.
--   bill/bill_detail 保留为后续月度汇总作业的产物,本表是出账的事实来源.
--
-- Sharding: account_id 单键, trade_db 8库×16表 (04§6.3). 金额一律 DECIMAL 禁止浮点 (03§6).
-- Idempotent: CREATE TABLE IF NOT EXISTS.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- settle_charge — 逐 ChargeID 的结算计划与结算结果
--
-- uk_acc_charge (account_id, charge_id) 是出账幂等的终裁: ChargeID = chg-{agg_id},
-- 两个副本不可能为同一聚合各建一份计划. status=1 (PLANNED) 时记录的是计划进度:
--   pack_intent  瀑布向各资源包申请的额度 —— 在第一笔资源包消耗之前落盘,
--                崩溃后据此按同一 ChargeID 重放消耗(资源包流水幂等),绝不重算;
--   pack_taken   资源包实际覆盖额度(0=未覆盖,份额顺延至券/现金),packs_done=1 后钉死;
-- status=2 (SETTLED) 时其余列即最终 Charge(含抵扣明细 deductions),任何副本的
-- 重放都原样返回,不再动资源包或现金. 已结算行不可再更新 (UPDATE ... AND status=1).
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `settle_charge` (
  `id`            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '代理主键;业务身份是 uk_acc_charge',
  `account_id`    BIGINT UNSIGNED NOT NULL COMMENT '分片键(≡uid≡user_id≡tenant_id)',
  `charge_id`     VARCHAR(64) NOT NULL COMMENT 'chg-{agg_id};出账幂等键,同现金流水 idempotency_key',
  `status`        TINYINT NOT NULL DEFAULT 1 COMMENT '1结算中(计划已落盘) 2已结算(终态,不可改)',
  `packs_done`    TINYINT(1) NOT NULL DEFAULT 0 COMMENT '1=资源包阶段完成,pack_taken 钉死',
  `pack_intent`   JSON DEFAULT NULL COMMENT '{pack_id: 申请额度};消耗前落盘',
  `pack_taken`    JSON DEFAULT NULL COMMENT '{pack_id: 实耗额度};0=未覆盖',
  `cash_paid`     DECIMAL(20,6) NOT NULL DEFAULT 0 COMMENT '本 charge 现金扣款;现金流水为准',
  `resource_id`   VARCHAR(64) NOT NULL DEFAULT '' COMMENT '资源ID;跨库引用不 JOIN',
  `product_code`  VARCHAR(32) NOT NULL DEFAULT '',
  `metering_item` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '计量项: cpu_core_hour 等',
  `bill_period`   CHAR(7) NOT NULL DEFAULT '' COMMENT '账期 YYYY-MM (UTC)',
  `billing_cycle` DATETIME DEFAULT NULL COMMENT '计费小时起点 (UTC)',
  `quantity`      DECIMAL(20,6) NOT NULL DEFAULT 0 COMMENT '用量;micro-units',
  `unit_price`    DECIMAL(20,6) NOT NULL DEFAULT 0 COMMENT '单价快照;不读当前价(03§4.2.4)',
  `pretax_amount` DECIMAL(20,6) NOT NULL DEFAULT 0 COMMENT '抵扣前金额',
  `pay_amount`    DECIMAL(20,6) NOT NULL DEFAULT 0 COMMENT '现金支付部分',
  `shortfall`     DECIMAL(20,6) NOT NULL DEFAULT 0 COMMENT '欠费部分;抵扣+欠费=应付',
  `covered_ratio` INT NOT NULL DEFAULT 0 COMMENT '计量覆盖率(聚合原样);<100 为不完整账单行',
  `snapshot_id`   VARCHAR(64) NOT NULL DEFAULT '' COMMENT '价格快照ID',
  `deductions`    JSON DEFAULT NULL COMMENT '[{source,ref,amount}] 抵扣明细(资源包→代金券→余额)',
  `settled_at`    DATETIME(6) DEFAULT NULL COMMENT '结算时间 (UTC);status=2 必填',
  `version`       INT NOT NULL DEFAULT 0 COMMENT '乐观锁;计划推进带 WHERE version=? AND status=1',
  `created_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_acc_charge` (`account_id`,`charge_id`) COMMENT '出账幂等;重试/重放不重复扣费',
  KEY `idx_acc_period` (`account_id`,`status`,`bill_period`) COMMENT '账单/成本分析按账期汇总',
  CONSTRAINT `ck_settle_amounts` CHECK (`cash_paid` >= 0 AND `pretax_amount` >= 0 AND `pay_amount` >= 0 AND `shortfall` >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='出账结算记录;ChargeID 幂等,计划进度可跨副本/重启续作(03§4.2.4)';

-- -----------------------------------------------------------------------------
-- settle_lock — 账户级出账串行锁
--
-- 每账户一行,结算时 SELECT ... FOR UPDATE 持有: 同一账户的结算(读余额 → 计算
-- 瀑布 → 提交)在所有副本间串行,取代进程内互斥锁. 行在首次结算时 INSERT IGNORE
-- 创建,不承载业务数据.
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `settle_lock` (
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '分片键;每账户一行',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='账户级出账锁;跨副本串行化同一账户的结算';
