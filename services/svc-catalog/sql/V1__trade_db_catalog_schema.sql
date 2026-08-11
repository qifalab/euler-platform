-- =============================================================================
-- svc-catalog DDL — trade_db (商品化中台)
--
-- Sharding: trade_db is 8 库 × 16 表 by account_id (04§6.3 lock box). The
-- catalogue tables below are the exception: t_product / t_sku / t_pricing_rule
-- / t_promo_policy are GLOBAL (broadcast) tables — they hold no tenant data and
-- every shard needs them for 询价. Only t_coupon and t_price_snapshot carry
-- account_id and shard with it.
--
-- Conventions (03 附录A DDL 评审模板): lowercase snake_case; uk_/idx_ prefixes;
-- money DECIMAL never float; DATETIME + UTC; TINYINT enums with COMMENT
-- dictionaries; created_at/updated_at everywhere; no cross-database JOIN.
--
-- Source of truth: 01-product-catalog.md §7 (domain model, tables, pricing
-- rules), 03-backend-services.md §4.2.1 (svc-catalog responsibilities).
-- =============================================================================

-- -----------------------------------------------------------------------------
-- t_product — 产品注册表
--
-- Decision D2 (01§1.1): 产品 = 资源类型 + 计量项 + OpenAPI, 三要素不齐不上架.
-- A product cannot reach status=2 (在售) until it has registered a resource
-- type here, at least one metering item in t_metering_item, and its OpenAPI
-- schema in svc-api-meta. The gateway refuses to mount routes for products
-- that are not registered, so this table is a real gate rather than a record.
--
-- 对标启示 2: 计量计费先于产品规划 — a product that ships without metering
-- fields costs 5x+ to retrofit, which is why the gate sits at registration
-- rather than at launch review.
-- -----------------------------------------------------------------------------
CREATE TABLE `t_product` (
  `product_code`  VARCHAR(32) NOT NULL COMMENT '产品代号,sc 前缀全小写(01§1.1 D0);创建后不可改',
  `product_name`  VARCHAR(64) NOT NULL COMMENT '显示名,如"辰云服务器"',
  `category`      VARCHAR(32) NOT NULL COMMENT '一级分类 compute/storage/network/database/middleware/monitor/security',
  `description`   VARCHAR(512) DEFAULT NULL COMMENT '一句话定位;直接驱动官网详情页渲染,运营不手写页面(01§3)',
  `resource_type` VARCHAR(32) NOT NULL COMMENT '资源类型;与 svc-orchestrator resource_instance.resource_type 对应',
  `region_scope`  VARCHAR(16) NOT NULL DEFAULT 'REGIONAL' COMMENT 'REGIONAL 分地域售卖 / GLOBAL 全局资源(如 IAM)',
  `status`        TINYINT NOT NULL DEFAULT 0 COMMENT '0草稿 1审核中 2在售 3停售;仅 2 可下单',
  `owner_team`    VARCHAR(64) DEFAULT NULL COMMENT '产品线归属团队',
  `created_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`product_code`),
  KEY `idx_category_status` (`category`,`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='产品注册表(全局广播表);三件套齐备方可 status=2 在售(决策 D2)';

-- -----------------------------------------------------------------------------
-- t_metering_item — 计量项注册
--
-- The second leg of the 三件套. 05-data-observability owns the collection
-- pipeline; this table is the commercial contract: what is measured, at what
-- precision, and how it is collected.
--
-- item_code format {productCode}.{metric}, e.g. scoss.storage_bytes (01§1.1).
-- -----------------------------------------------------------------------------
CREATE TABLE `t_metering_item` (
  `item_code`      VARCHAR(64) NOT NULL COMMENT '计量项代号 {productCode}.{metric},如 scoss.storage_bytes',
  `product_code`   VARCHAR(32) NOT NULL,
  `metric_name`    VARCHAR(64) NOT NULL COMMENT '指标名,如 cpu_seconds / storage_gb_hour',
  `unit`           VARCHAR(16) NOT NULL COMMENT '单位 second/gb_hour/count',
  `precision_unit` VARCHAR(8) NOT NULL DEFAULT 'HOUR' COMMENT 'SECOND/MINUTE/HOUR;按量计费精度到秒,出账按小时聚合',
  `collect_mode`   VARCHAR(16) NOT NULL COMMENT 'PUSH 控制面上报 / PULL 轮询',
  `collect_period` INT NOT NULL DEFAULT 60 COMMENT '采集周期(秒)',
  `billing_phase`  VARCHAR(16) NOT NULL COMMENT '服务于哪种计费形态',
  `status`         TINYINT NOT NULL DEFAULT 1 COMMENT '1启用 2停用',
  `created_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`item_code`),
  KEY `idx_product` (`product_code`,`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='计量项注册(全局广播表);不可计量的产品不允许上架(架构原则 6)';

-- -----------------------------------------------------------------------------
-- t_sku — 售卖单元 = 产品 + 规格组合 + 计费形态
-- -----------------------------------------------------------------------------
CREATE TABLE `t_sku` (
  `sku_code`     VARCHAR(64) NOT NULL COMMENT 'SKU 代号,如 scecs.s2.large.prepaid',
  `product_code` VARCHAR(32) NOT NULL,
  `charge_type`  VARCHAR(16) NOT NULL COMMENT 'PREPAID 包年包月 / POSTPAID 按量;RESOURCE_PACK/SPOT 模型预留,一期不售(D6)',
  `spec_json`    JSON NOT NULL COMMENT '规格,如 {"cpu":2,"mem_gb":4}',
  `status`       TINYINT NOT NULL DEFAULT 0 COMMENT '0草稿 1在售 2下架',
  `created_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`sku_code`),
  KEY `idx_product_status` (`product_code`,`status`),
  KEY `idx_charge_type` (`charge_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='SKU(全局广播表);一期仅 PREPAID/POSTPAID 可置 status=1(决策 D6)';

-- -----------------------------------------------------------------------------
-- t_pricing_rule — 定价规则
--
-- APPEND-ONLY. A price change inserts a new row with a new effective range and
-- closes the old row's effective_to; it never UPDATEs list_price in place.
--
-- This is the 资损防控底线 (01§12.3 rule 3): existing orders are settled
-- against their frozen price snapshot, and any historical bill must remain
-- explicable years later. An in-place price edit silently rewrites history and
-- makes 对账 differences unresolvable — which 09 A2 (无未解释差异) forbids.
--
-- Rule selection specificity (implemented in pkg-go/pricing):
--   region-specific beats wildcard; customer-level beats unscoped.
-- -----------------------------------------------------------------------------
CREATE TABLE `t_pricing_rule` (
  `rule_id`        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `sku_code`       VARCHAR(64) NOT NULL,
  `region_id`      VARCHAR(32) NOT NULL DEFAULT '*' COMMENT '* 表示不限地域;具体地域优先级更高',
  `duration_unit`  VARCHAR(8) NOT NULL DEFAULT 'MONTH' COMMENT 'MONTH/YEAR/HOUR/USAGE',
  `list_price`     DECIMAL(12,6) NOT NULL COMMENT '目录价;DECIMAL 禁止浮点(03§6 约定)',
  `currency`       CHAR(3) NOT NULL DEFAULT 'CNY' COMMENT '多币种 Day1 预留(架构原则 12)',
  `customer_level` VARCHAR(16) NOT NULL DEFAULT '' COMMENT '客户等级差价;空串表示不限',
  `effective_from` DATETIME NOT NULL COMMENT '生效起(UTC)',
  `effective_to`   DATETIME DEFAULT NULL COMMENT '生效止;NULL 表示当前有效',
  `created_by`     VARCHAR(64) DEFAULT NULL COMMENT '变更人,审计用',
  `created_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`rule_id`),
  KEY `idx_sku_region` (`sku_code`,`region_id`,`effective_from`),
  KEY `idx_effective` (`effective_from`,`effective_to`) COMMENT '生效期扫描'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='定价规则(全局广播表);只增不改——价格可追溯、不可改写是资损防控底线(01§12.3)';

-- -----------------------------------------------------------------------------
-- t_promo_policy — 促销策略
-- Promotions never stack: 询价 takes the single best applicable one (01§12.3).
-- -----------------------------------------------------------------------------
CREATE TABLE `t_promo_policy` (
  `promo_id`     VARCHAR(32) NOT NULL,
  `promo_name`   VARCHAR(64) NOT NULL,
  `promo_type`   VARCHAR(16) NOT NULL COMMENT 'DISCOUNT_RATE 折扣率 / FIXED_PRICE 一口价;COUPON 走 t_coupon',
  `scope_type`   VARCHAR(16) NOT NULL COMMENT 'PRODUCT/SKU/ORDER',
  `scope_ref`    VARCHAR(64) NOT NULL COMMENT '作用对象;scope_type=ORDER 时为空',
  `rate_bp`      INT DEFAULT NULL COMMENT '折扣率(基点,8500=85折);promo_type=DISCOUNT_RATE 时必填',
  `fixed_price`  DECIMAL(12,6) DEFAULT NULL COMMENT '一口价;promo_type=FIXED_PRICE 时必填',
  `user_tag`     VARCHAR(32) NOT NULL DEFAULT '' COMMENT 'new/enterprise 等;空串表示不限人群',
  `budget_total` DECIMAL(14,2) DEFAULT NULL COMMENT '预算上限,超出停止发放',
  `budget_used`  DECIMAL(14,2) NOT NULL DEFAULT 0 COMMENT '已消耗预算',
  `start_at`     DATETIME NOT NULL,
  `end_at`       DATETIME NOT NULL,
  `status`       TINYINT NOT NULL DEFAULT 1 COMMENT '1启用 2停用',
  `created_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`promo_id`),
  KEY `idx_scope` (`scope_type`,`scope_ref`,`status`),
  KEY `idx_window` (`start_at`,`end_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='促销策略(全局广播表);不叠加,询价取最优一条(01§12.3)';

-- -----------------------------------------------------------------------------
-- t_coupon — 代金券
--
-- The carrier for 免费试用 (decision D7): a trial is a voucher applied through
-- the STANDARD postpaid order flow, not a separate free-tier inventory system.
-- Trial resources therefore exercise the same metering → billing → 停服 pipeline
-- as paid ones, which is what makes the trial a real rehearsal of the billing
-- link and gives zero-friction trial-to-paid conversion.
--
-- 满减券/折扣券 are deferred to phase 2; phase 1 ships the 定额抵扣 minimal
-- implementation only.
--
-- SHARDED by account_id (owner_account_id column).
-- -----------------------------------------------------------------------------
CREATE TABLE `t_coupon` (
  `coupon_id`         VARCHAR(32) NOT NULL,
  `account_id`        BIGINT UNSIGNED NOT NULL COMMENT '归属租户;分片键(≡uid≡user_id≡tenant_id)',
  `face_value`        DECIMAL(10,2) NOT NULL COMMENT '面额',
  `remain_value`      DECIMAL(10,2) NOT NULL COMMENT '剩余可抵扣;抵扣按到期时间升序消耗',
  `scope_json`        JSON DEFAULT NULL COMMENT '{"product_codes":[],"charge_types":[],"sku_codes":[]};空表示不限',
  `source`            VARCHAR(32) NOT NULL DEFAULT 'TRIAL' COMMENT 'TRIAL 试用券/PROMO 活动发放/COMPENSATE 故障补偿',
  `promo_id`          VARCHAR(32) DEFAULT NULL COMMENT '来源活动',
  `status`            TINYINT NOT NULL DEFAULT 0 COMMENT '0未用 1部分使用 2用尽 3过期',
  `expire_at`         DATETIME NOT NULL COMMENT '到期时间;领取后 30 天内使用(01§10 试用参数)',
  `version`           INT NOT NULL DEFAULT 0 COMMENT '乐观锁;抵扣扣减依赖',
  `created_at`        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`coupon_id`),
  KEY `idx_owner_status` (`account_id`,`status`,`expire_at`) COMMENT '抵扣时按到期升序取券',
  KEY `idx_expire` (`expire_at`,`status`) COMMENT '过期回收扫描'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='代金券;免费试用的载体(决策 D7);现金余额与流水归 trade_db ledger(裁决 S29)';

-- -----------------------------------------------------------------------------
-- t_price_snapshot — 价格快照
--
-- Frozen at 下单 time and referenced by every downstream step: 出账 computes
-- against the snapshot, 对账 explains differences against it, 退订 折算 uses it
-- rather than the current price.
--
-- IMMUTABLE: no UPDATE path exists. A correction is a new snapshot plus an
-- adjustment record, never an edit — the snapshot is the evidence that a given
-- charge was correct at the moment the customer agreed to it (01§12.3 rule 4).
--
-- SHARDED by account_id.
-- -----------------------------------------------------------------------------
CREATE TABLE `t_price_snapshot` (
  `snapshot_id`    VARCHAR(32) NOT NULL,
  `account_id`     BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `order_id`       BIGINT UNSIGNED DEFAULT NULL COMMENT '下单后回填;询价阶段为 NULL',
  `sku_code`       VARCHAR(64) NOT NULL,
  `region_id`      VARCHAR(32) NOT NULL,
  `charge_type`    VARCHAR(16) NOT NULL,
  `quantity`       INT NOT NULL DEFAULT 1,
  `duration`       INT DEFAULT NULL COMMENT '时长;按量为 NULL',
  `duration_unit`  VARCHAR(8) DEFAULT NULL,
  `rule_id`        BIGINT UNSIGNED NOT NULL COMMENT '所用定价规则;历史订单据此可解释',
  `list_amount`    DECIMAL(12,6) NOT NULL COMMENT '目录价小计',
  `promo_amount`   DECIMAL(12,6) NOT NULL DEFAULT 0 COMMENT '促销抵扣',
  `promo_id`       VARCHAR(32) DEFAULT NULL COMMENT '所用促销;不叠加故仅一条',
  `coupon_amount`  DECIMAL(12,6) NOT NULL DEFAULT 0 COMMENT '代金券抵扣合计',
  `payable_amount` DECIMAL(12,6) NOT NULL COMMENT '应付;= list - promo - coupon,且不小于 0',
  `detail_json`    JSON NOT NULL COMMENT '完整计价过程含逐张券消耗明细;客诉与审计依据',
  `expire_at`      DATETIME NOT NULL COMMENT '报价有效期;超时需重新询价,防止拿旧价下单',
  `created_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`snapshot_id`),
  KEY `idx_account_order` (`account_id`,`order_id`),
  KEY `idx_expire` (`expire_at`) COMMENT '过期报价清理'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='价格快照;下单时冻结,出账/对账/退订折算一律以此为准,禁止 UPDATE(01§12.3)';

-- -----------------------------------------------------------------------------
-- 注册完整性校验视图
--
-- Encodes the D2 三件套 gate as a query so the launch checklist and CI can both
-- assert it: a product may only go 在售 when it has a resource type, at least
-- one enabled metering item, and at least one on-sale SKU with a current price.
-- OpenAPI registration lives in svc-api-meta and is checked by the pipeline
-- gate there.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE VIEW `v_product_launch_readiness` AS
SELECT
  p.product_code,
  p.product_name,
  p.status,
  (p.resource_type IS NOT NULL AND p.resource_type <> '')            AS has_resource_type,
  (SELECT COUNT(*) FROM t_metering_item m
     WHERE m.product_code = p.product_code AND m.status = 1)         AS metering_item_count,
  (SELECT COUNT(*) FROM t_sku s
     WHERE s.product_code = p.product_code AND s.status = 1)         AS on_sale_sku_count,
  (SELECT COUNT(*) FROM t_sku s
     JOIN t_pricing_rule r ON r.sku_code = s.sku_code
     WHERE s.product_code = p.product_code
       AND s.status = 1
       AND r.effective_from <= UTC_TIMESTAMP()
       AND (r.effective_to IS NULL OR r.effective_to > UTC_TIMESTAMP())) AS priced_sku_count
FROM t_product p;
