-- =============================================================================
-- svc-catalog V3 — B7 运营能力 (09-roadmap §4.4 B7): 券体系扩展 + 免费试用防薅
--
-- Two deliverables in one migration, because they share one story:
--   1. t_coupon gains the phase-2 券类型 (满减券/折扣券) the pricing engine
--      already computes with (pkg-go/pricing ③a/③b/③c fixed sub-order).
--   2. The trial admission tables backing pkg-go/trial — the claim history,
--      the cross-account identity registry, and the activity budget.
--
-- A trial is a voucher claimed through the standard order flow (decision D7),
-- which is exactly why the claim needs a gate: without admission control,
-- 免费试用 is an unlimited subsidy payable to anyone willing to register again
-- (01-product-catalog.md §10 试用参数).
--
-- Conventions (03 附录A DDL 评审模板): lowercase snake_case; uk_/idx_ prefixes;
-- money DECIMAL never float; DATETIME + UTC; TINYINT enums with COMMENT
-- dictionaries; created_at/updated_at everywhere; no cross-database JOIN.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- t_coupon: phase-2 kind columns (B7-1)
--
-- Phase-1 shipped the 定额抵扣 minimal form only; the new columns default so
-- existing rows stay valid without a data migration ('' kind → VOUCHER in
-- pkg-go/pricing.Coupon.kind()). Coupon scope sub-rules and non-stacking are
-- enforced by the engine, not the schema: one rate and one threshold coupon per
-- order is a pricing-pipeline invariant, not a table constraint.
--
--   VOUCHER   代金券 — 定额抵扣, 可多张叠加, 按到期升序消耗 (D7 试用载体)
--   THRESHOLD 满减券 — 满 threshold 减 face_value, 单张
--   RATE      折扣券 — 支付 rate_bp 的比例 (8500=85折), 单张, 折扣封顶 cap_amount
--
-- cap_amount is the 防资损 cap: a "9折" coupon on a huge order without a cap is
-- an unbounded liability. Zero means uncapped — allowed for migration but a
-- config smell; seeded activities always set caps.
-- -----------------------------------------------------------------------------
ALTER TABLE `t_coupon`
  ADD COLUMN `kind`       VARCHAR(16) NOT NULL DEFAULT 'VOUCHER'
    COMMENT '券类型 VOUCHER代金券/THRESHOLD满减券/RATE折扣券;空串按VOUCHER(一期兼容)' AFTER `coupon_id`,
  ADD COLUMN `threshold`  DECIMAL(10,2) DEFAULT NULL
    COMMENT '满减券门槛 满 X;kind=THRESHOLD 必填' AFTER `face_value`,
  ADD COLUMN `rate_bp`    INT DEFAULT NULL
    COMMENT '折扣券留存基点 8500=85折;kind=RATE 必填,取值(0,10000)' AFTER `threshold`,
  ADD COLUMN `cap_amount` DECIMAL(10,2) NOT NULL DEFAULT 0
    COMMENT '折扣封顶 防资损;kind=RATE 用,0表示不封顶(配置坏味道)' AFTER `rate_bp`;

-- The issuance query (满减/折扣券活动发放) filters by kind.
ALTER TABLE `t_coupon`
  ADD KEY `idx_kind_owner` (`kind`,`account_id`,`status`,`expire_at`);

-- -----------------------------------------------------------------------------
-- t_trial_activity — 试用活动与全局预算 (GLOBAL 广播表)
--
-- The anti-abuse Policy (pkg-go/trial) plus the platform-wide counters the
-- engine needs: GlobalActive (live vouchers) is maintained by the claim/consume
-- paths so the gate needs no money arithmetic and no cross-shard scan to
-- enforce 活动预算.
--
-- Dedup across accounts is keyed by identity_key = SHA-256(证件类型+证件号),
-- never the raw document — the registry must not become a second 身份证库.
-- -----------------------------------------------------------------------------
CREATE TABLE `t_trial_activity` (
  `activity_id`            VARCHAR(32) NOT NULL,
  `activity_name`          VARCHAR(64) NOT NULL COMMENT '活动名,如"新用户0元试用"',
  `policy_json`            JSON NOT NULL COMMENT 'pkg-go/trial.Policy 快照:实名/并发/终身/身份/冷静期',
  `voucher_face_value`     DECIMAL(10,2) NOT NULL COMMENT '试用券面额(元);领取即按面额发放 VOUCHER 券',
  `voucher_valid_days`     INT NOT NULL DEFAULT 30 COMMENT '券有效期(天);01§10 领取后 30 天内使用',
  `max_active_global`      BIGINT NOT NULL DEFAULT 10000 COMMENT '平台并发活券上限=活动预算(以张数表达)',
  `active_count`           BIGINT NOT NULL DEFAULT 0 COMMENT '当前活券数;领取+1,消耗/过期-1',
  `require_real_name`      TINYINT(1) NOT NULL DEFAULT 1 COMMENT '是否要求实名(0否 1是);身份去重的前提',
  `max_per_identity`       INT NOT NULL DEFAULT 1 COMMENT '每实名身份(跨账号)总领取上限',
  `status`                 TINYINT NOT NULL DEFAULT 1 COMMENT '1进行中 2停发 3结束',
  `start_at`               DATETIME NOT NULL,
  `end_at`                 DATETIME NOT NULL,
  `created_at`             DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`             DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`activity_id`),
  KEY `idx_status_window` (`status`,`start_at`,`end_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='试用活动(全局广播表);预算以活券张数表达,领取消耗原位更新';

-- -----------------------------------------------------------------------------
-- t_trial_record — 试用领取记录 (SHARDED by account_id)
--
-- One row per claim. The account-scoped counters the engine reads (active /
-- lifetime / last_claimed_at) are derived from this table; recording a claim
-- inserts here AND bumps t_trial_activity.active_count AND t_trial_identity
-- .claim_count in one transaction (svc-order 领取端点), so the three views of
-- the same event cannot drift.
--
-- state lifecycle: ACTIVE(已领取未消耗) → CONSUMED(用尽/转化) | EXPIRED(过期未用)
-- 消耗 is triggered by the billing pipeline using the voucher, mirroring D7:
-- the trial exercises the same metering → billing path as paid resources.
-- -----------------------------------------------------------------------------
CREATE TABLE `t_trial_record` (
  `record_id`      BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `activity_id`    VARCHAR(32) NOT NULL,
  `account_id`     BIGINT UNSIGNED NOT NULL COMMENT '领取租户;分片键(≡uid≡user_id≡tenant_id)',
  `coupon_id`      VARCHAR(32) NOT NULL COMMENT '发放的试用券;t_coupon.source=TRIAL',
  `identity_key`   CHAR(64) NOT NULL COMMENT 'SHA-256(证件类型+证件号);跨账号去重键,绝不存明文证件',
  `state`          TINYINT NOT NULL DEFAULT 0 COMMENT '0 ACTIVE已领取 1 CONSUMED已消耗 2 EXPIRED过期',
  `claimed_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '领取时刻;冷静期基准',
  `settled_at`     DATETIME DEFAULT NULL COMMENT '消耗/过期时刻',
  `created_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`record_id`),
  UNIQUE KEY `uk_account_activity` (`account_id`,`activity_id`) COMMENT '并发/终身上限的表级兜底',
  KEY `idx_account_claimed` (`account_id`,`state`,`claimed_at`) COMMENT '账号侧计数扫描',
  KEY `idx_identity` (`identity_key`) COMMENT '身份侧核对(权威计数在 t_trial_identity)'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='试用领取记录(分片表);账号侧计数来源,与活动计数/身份计数同事务更新';

-- -----------------------------------------------------------------------------
-- t_trial_identity — 实名身份领取登记 (GLOBAL 广播表)
--
-- The one table that can stop "注册 N 个账号领 N 张券": identity_key → total
-- claims ACROSS accounts. It is global (not sharded) precisely because the
-- attack crosses shard boundaries; contention is a single hot row per real
-- person, which the claim transaction serializes via the row lock.
-- -----------------------------------------------------------------------------
CREATE TABLE `t_trial_identity` (
  `identity_key`   CHAR(64) NOT NULL COMMENT 'SHA-256(证件类型+证件号);与 t_trial_record.identity_key 同源',
  `claim_count`    INT NOT NULL DEFAULT 0 COMMENT '该身份累计领取次数(跨账号)',
  `last_account`   BIGINT UNSIGNED NOT NULL COMMENT '最近领取账号;风控画像,非约束',
  `last_claimed_at` DATETIME NOT NULL,
  `created_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`identity_key`),
  KEY `idx_last_claimed` (`last_claimed_at`) COMMENT '冷静期/风控扫描'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='实名身份领取登记(全局广播表);跨账号防薅的唯一防线,pkg-go/trial 身份规则的数据源';

-- -----------------------------------------------------------------------------
-- 种子: 默认试用活动(与 pkg-go/trial.DefaultPolicy 对齐)
--
-- Illustrative figures like V2; real activity parameters come from 运营评审.
-- -----------------------------------------------------------------------------
INSERT INTO `t_trial_activity`
  (`activity_id`,`activity_name`,`policy_json`,`voucher_face_value`,`voucher_valid_days`,
   `max_active_global`,`require_real_name`,`max_per_identity`,`status`,`start_at`,`end_at`) VALUES
  ('trial-default','新用户0元试用',
   '{"RequireRealName":true,"MaxActivePerAccount":1,"MaxLifetimePerAccount":1,"MaxPerIdentity":1,"CooldownHours":720}',
   100.00, 30, 10000, 1, 1, 1, '2026-01-01 00:00:00', '2026-12-31 23:59:59');

-- -----------------------------------------------------------------------------
-- 注册完整性校验视图的 B7 对应物: 试用发放健康度
--
-- Encodes the gate invariants as a query for the ops console and CI:
--   live vouchers (active_count) must never exceed max_active_global,
--   and identity claims must never exceed per-identity cap.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE VIEW `v_trial_health` AS
SELECT
  a.activity_id,
  a.activity_name,
  a.status,
  a.max_active_global,
  a.active_count,
  (a.active_count <= a.max_active_global) AS budget_ok,
  (SELECT COUNT(*) FROM t_trial_identity i
     WHERE i.claim_count > a.max_per_identity) AS identity_overclaims
FROM t_trial_activity a;
