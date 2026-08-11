-- =============================================================================
-- svc-metering + svc-billing DDL — metering_db / trade_db
--
-- Sharding: account_id single key, 8 库 × 16 表 (04§6.3).
--
-- Detail data lives in ClickHouse; MySQL holds the hourly aggregate and the
-- bill (05§2.1). The split is deliberate: raw readings are append-only,
-- high-volume and analytical (columnar wins); bills are transactional and must
-- join with orders and balances (row store wins).
--
-- Source of truth: 05-data-observability.md §5 (metering pipeline, §5.5
-- idempotency & reconciliation, §5.6 freeze), 03-backend-services.md §6.4.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- metering_catalog — 计量项注册(全局广播表)
--
-- Schema-registration discipline (05§5.3): a product registers its resource
-- type and metering items BEFORE launch. 对标启示 2 puts the reason plainly —
-- retrofitting metering fields onto a shipped product costs 5x, and until they
-- exist the product cannot be billed at all.
-- -----------------------------------------------------------------------------
CREATE TABLE `metering_catalog` (
  `resource_type`  VARCHAR(32) NOT NULL COMMENT 'ecs/oss/rds/...',
  `metering_item`  VARCHAR(64) NOT NULL COMMENT 'cpu_core_hour/storage_gb_hour/...',
  `unit`           VARCHAR(16) NOT NULL COMMENT 'second/gb_hour/count',
  `precision_digits` TINYINT NOT NULL DEFAULT 6 COMMENT '小数位;与 DECIMAL(18,6) 对齐',
  `collect_source` VARCHAR(16) NOT NULL COMMENT 'agent 主机采集 / control_plane 控制面上报',
  `collect_period` INT NOT NULL DEFAULT 60 COMMENT '采集周期(秒);决定每小时期望窗口数',
  `status`         TINYINT NOT NULL DEFAULT 1 COMMENT '1启用 2停用',
  `created_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`resource_type`,`metering_item`),
  KEY `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='计量项注册;不可计量的产品不允许上架(架构原则6)';

-- -----------------------------------------------------------------------------
-- metering_record — 小时聚合结果
--
-- The unique key (resource_id, metering_item, metering_hour) is what makes
-- aggregation infinitely re-runnable (03§4.2.5 聚合任务失败可无限重算): a
-- re-run upserts the same row rather than appending a second total. Without
-- it, every retry after a partial failure would inflate the bill.
--
-- covered_ratio carries the honesty signal forward. An hour aggregated from
-- 45 of 60 expected windows is an undercount, and publishing it without
-- saying so makes an incomplete hour indistinguishable from a quiet one.
-- -----------------------------------------------------------------------------
CREATE TABLE `metering_record` (
  `id`             BIGINT UNSIGNED NOT NULL,
  `agg_id`         VARCHAR(32) NOT NULL COMMENT 'sha1(resource_id|item|hour)[:24];幂等键,派生而非随机',
  `account_id`     BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `resource_id`    VARCHAR(64) NOT NULL,
  `resource_type`  VARCHAR(32) NOT NULL,
  `region`         VARCHAR(32) NOT NULL,
  `metering_item`  VARCHAR(64) NOT NULL,
  `metering_hour`  DATETIME NOT NULL COMMENT '整点;计量周期',
  `quantity`       DECIMAL(18,6) NOT NULL COMMENT '用量;定点数,浮点累加会漂移',
  `covered_ratio`  TINYINT NOT NULL DEFAULT 100 COMMENT '实收窗口/期望窗口 ×100;<100 触发补采(05§5.5.2 L1)',
  `windows_seen`   INT NOT NULL DEFAULT 0,
  `windows_expected` INT NOT NULL DEFAULT 60,
  `source`         TINYINT NOT NULL COMMENT '1推送 2拉取 3补算',
  `batch_id`       VARCHAR(16) NOT NULL DEFAULT 'rt' COMMENT 'rt 实时 / late 迟到 / backfill 补采',
  `created_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_res_item_hour` (`resource_id`,`metering_item`,`metering_hour`) COMMENT '幂等重算依赖此键',
  UNIQUE KEY `uk_agg_id` (`agg_id`),
  KEY `idx_acc_hour` (`account_id`,`metering_hour`) COMMENT '出账扫描',
  KEY `idx_incomplete` (`covered_ratio`,`metering_hour`) COMMENT '补采任务扫描'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='小时计量聚合;计量宁可重采不可漏采,去重靠派生幂等键';

-- -----------------------------------------------------------------------------
-- metering_backfill_task — 补采任务
-- -----------------------------------------------------------------------------
CREATE TABLE `metering_backfill_task` (
  `task_id`       BIGINT UNSIGNED NOT NULL,
  `account_id`    BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `resource_id`   VARCHAR(64) NOT NULL,
  `metering_item` VARCHAR(64) NOT NULL,
  `metering_hour` DATETIME NOT NULL,
  `covered_ratio` TINYINT NOT NULL COMMENT '触发补采时的覆盖率',
  `missing_count` INT NOT NULL COMMENT '缺失窗口数',
  `status`        VARCHAR(16) NOT NULL DEFAULT 'PENDING' COMMENT 'PENDING/RUNNING/DONE/ABANDONED',
  `attempt`       INT NOT NULL DEFAULT 0,
  `last_error`    VARCHAR(512) DEFAULT NULL,
  `created_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`task_id`),
  UNIQUE KEY `uk_target` (`resource_id`,`metering_item`,`metering_hour`) COMMENT '同一小时只补一次',
  KEY `idx_status` (`status`,`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='补采任务;来源于 covered_ratio<100';

-- -----------------------------------------------------------------------------
-- bill_detail — 小时账单明细
--
-- charge_id derives from agg_id, so re-settling an hour produces the same id
-- and the unique index makes settlement idempotent (05§5.5.1). The snapshot_id
-- column is what keeps a bill explicable years later: the charge is anchored to
-- the price the customer agreed to, not to whatever the catalogue says today
-- (01§12.3 rule 4).
-- -----------------------------------------------------------------------------
CREATE TABLE `bill_detail` (
  `id`             BIGINT UNSIGNED NOT NULL,
  `charge_id`      VARCHAR(32) NOT NULL COMMENT 'chg-{agg_id};结算幂等键',
  `account_id`     BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `bill_period`    CHAR(7) NOT NULL COMMENT '2026-08 月账期',
  `billing_cycle`  DATETIME NOT NULL COMMENT '小时计费周期',
  `resource_id`    VARCHAR(64) NOT NULL,
  `product_code`   VARCHAR(32) NOT NULL,
  `metering_item`  VARCHAR(64) NOT NULL,
  `quantity`       DECIMAL(18,6) NOT NULL,
  `unit_price`     DECIMAL(12,6) NOT NULL COMMENT '取自价格快照,不读当前价',
  `snapshot_id`    VARCHAR(32) NOT NULL COMMENT '价格快照;历史账单据此可解释',
  `pretax_amount`  DECIMAL(12,6) NOT NULL COMMENT '原价',
  `deduct_package` DECIMAL(12,6) NOT NULL DEFAULT 0 COMMENT '资源包抵扣(一期恒为0,D6)',
  `deduct_coupon`  DECIMAL(12,6) NOT NULL DEFAULT 0 COMMENT '代金券抵扣',
  `pay_amount`     DECIMAL(12,6) NOT NULL COMMENT '实付(扣余额)',
  `shortfall`      DECIMAL(12,6) NOT NULL DEFAULT 0 COMMENT '未覆盖金额;>0 即欠费,由生命周期状态机接管',
  `covered_ratio`  TINYINT NOT NULL DEFAULT 100 COMMENT '<100 表示本行基于不完整计量,账单为暂定',
  `deduction_detail` JSON DEFAULT NULL COMMENT '逐笔抵扣明细含券号;客诉举证依据',
  `settled_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_charge_id` (`charge_id`) COMMENT '重复结算被拒',
  KEY `idx_acc_period` (`account_id`,`bill_period`),
  KEY `idx_resource_cycle` (`resource_id`,`billing_cycle`),
  KEY `idx_incomplete` (`covered_ratio`,`bill_period`) COMMENT '暂定账单扫描'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='小时账单明细;6个月后归档 ClickHouse';

-- -----------------------------------------------------------------------------
-- bill_main — 月度汇总账单
-- -----------------------------------------------------------------------------
CREATE TABLE `bill_main` (
  `bill_id`        BIGINT UNSIGNED NOT NULL,
  `account_id`     BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `bill_period`    CHAR(7) NOT NULL,
  `total_amount`   DECIMAL(14,2) NOT NULL COMMENT '原价合计',
  `deduct_amount`  DECIMAL(14,2) NOT NULL DEFAULT 0 COMMENT '抵扣合计',
  `paid_amount`    DECIMAL(14,2) NOT NULL DEFAULT 0 COMMENT '实付合计',
  `charge_count`   INT NOT NULL DEFAULT 0,
  `incomplete_count` INT NOT NULL DEFAULT 0 COMMENT '基于不完整计量的行数;>0 则账单为暂定不可出具',
  `unreconciled_count` INT NOT NULL DEFAULT 0 COMMENT '分项不平的行数;商业验收要求为 0(09 A2)',
  `status`         TINYINT NOT NULL COMMENT '1出账中 2已出账 3已结清 4欠费',
  `settled_at`     DATETIME DEFAULT NULL,
  `created_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`bill_id`),
  UNIQUE KEY `uk_acc_period` (`account_id`,`bill_period`),
  KEY `idx_status` (`status`,`bill_period`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='月度汇总账单;incomplete/unreconciled 非零不得出具正式账单';

-- -----------------------------------------------------------------------------
-- recon_report — 对账报表 (05§5.5.2 L2)
-- -----------------------------------------------------------------------------
CREATE TABLE `recon_report` (
  `id`             BIGINT UNSIGNED NOT NULL,
  `recon_date`     DATE NOT NULL COMMENT 'T+1 对账日',
  `account_id`     BIGINT UNSIGNED NOT NULL,
  `resource_id`    VARCHAR(64) NOT NULL,
  `expected_hours` INT NOT NULL COMMENT '按资源生命周期时间线推导的应计小时数',
  `actual_hours`   INT NOT NULL COMMENT '实际计量小时数',
  `diff_ratio`     DECIMAL(8,6) NOT NULL COMMENT '|expected-actual|/expected',
  `action`         VARCHAR(16) NOT NULL COMMENT 'NONE/BACKFILL(>0.1%)/ALERT(>0.5%)',
  `resolved`       TINYINT NOT NULL DEFAULT 0 COMMENT '0未闭环 1已闭环;商业验收要求全部闭环',
  `resolution`     VARCHAR(512) DEFAULT NULL COMMENT '差异原因说明;这就是"无未解释差异"的载体',
  `created_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_date_resource` (`recon_date`,`resource_id`),
  KEY `idx_unresolved` (`resolved`,`recon_date`) COMMENT '未闭环差异扫描'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='对账报表;阈值 0.1%/0.5% 是工程触发线,商业验收口径是无未解释差异(裁决S17)';

-- -----------------------------------------------------------------------------
-- 对账视图:计量不完整
-- -----------------------------------------------------------------------------
CREATE OR REPLACE VIEW `v_metering_incomplete` AS
SELECT
  m.account_id,
  m.resource_id,
  m.metering_item,
  m.metering_hour,
  m.covered_ratio,
  (m.windows_expected - m.windows_seen) AS missing_windows,
  m.batch_id,
  m.updated_at
FROM metering_record m
WHERE m.covered_ratio < 100
ORDER BY m.metering_hour DESC;

-- -----------------------------------------------------------------------------
-- 对账视图:账单行分项不平
--
-- 09 A2 requires 无未解释差异. A bill line whose deductions plus shortfall do
-- not equal its pretax amount is money that appeared or vanished between
-- pricing and settlement.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE VIEW `v_bill_detail_unreconciled` AS
SELECT
  d.id,
  d.charge_id,
  d.account_id,
  d.bill_period,
  d.resource_id,
  d.pretax_amount,
  (d.deduct_package + d.deduct_coupon + d.pay_amount + d.shortfall) AS component_sum,
  (d.pretax_amount - (d.deduct_package + d.deduct_coupon + d.pay_amount + d.shortfall)) AS difference,
  d.settled_at
FROM bill_detail d
WHERE d.pretax_amount <> (d.deduct_package + d.deduct_coupon + d.pay_amount + d.shortfall);

-- -----------------------------------------------------------------------------
-- 对账视图:运行中但无计量的资源
--
-- The silent revenue leak: a resource the lifecycle says is RUNNING but which
-- produced no metering rows. Nothing inside the metering pipeline can detect
-- this, because the pipeline never saw the resource at all — only comparing
-- against the resource ledger finds it.
--
-- Cross-database in the logical model (resource_db vs metering_db); in
-- production this comparison runs in the reconciliation job, which reads both
-- through their services rather than joining across shards.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE VIEW `v_metering_missing_hours` AS
SELECT
  r.recon_date,
  r.account_id,
  r.resource_id,
  r.expected_hours,
  r.actual_hours,
  (r.expected_hours - r.actual_hours) AS missing_hours,
  r.diff_ratio,
  r.action
FROM recon_report r
WHERE r.actual_hours < r.expected_hours
  AND r.resolved = 0;
