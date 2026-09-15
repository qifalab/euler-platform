-- =============================================================================
-- svc-billing DDL — trade_db (资源包抵扣账本, 二期 M-4.1)
--
-- 决策 D6 后置项,二期 M-4.1 上线. 资源包 = 预付购买固定额度的抵扣账本,
-- 在抵扣瀑布中 rank 0 优先于代金券/余额 (01§12.3: 资源包额度→代金券→现金).
-- 一期 billing.SourceResourcePack 瀑布 tier 与 bill_detail.charge_type=3 已预留,
-- 本 DDL 补齐"额度账户"与"append-only 流水",不动出账主链路.
--
-- Sharding: account_id 单键, trade_db 8库×16表 (04§6.3). 两表均带 account_id
-- 冗余分片键 —— 禁止跨库 JOIN,冗余比关联更便宜 (03 附录A).
-- 金额一律 DECIMAL 禁止浮点 (03§6);内存对应 pkg-go/reservepack (pricing.Amount
-- micro-units,DECIMAL(18,6) 对齐).
--
-- Source of truth: 01-product-catalog.md §5.1 D6, §12.3; 03-backend-services.md
-- §4.2.4; 09-roadmap.md §4.3 M-4.1.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- resource_pack — 资源包额度账本
--
-- One row per purchased pack. uk_pack makes the purchase idempotent: a replayed
-- order settlement credits the pack exactly once. version is the optimistic
-- lock consumed by every consume/refund/expire (mirrors ledger.account version
-- and resource_instance version).
--
-- Money invariants enforced in the schema:
--   * remaining ∈ [0, face_value] —— never negative, never exceeds what was
--     purchased. A refund may revive remaining but cannot exceed face_value.
--   * status is terminal-safe: EXHAUSTED/EXPIRED packs have remaining=0.
--   * expire_at past + status ACTIVE is a sweep candidate; the sweeper
--     (svc-billing /internal/reservepack/expire-sweep) transitions it to
--     EXPIRED, forfeiting residual quota.
-- -----------------------------------------------------------------------------
CREATE TABLE `resource_pack` (
  `pack_id`       VARCHAR(64) NOT NULL COMMENT '资源包ID;购买订单生成,全局唯一',
  `account_id`    BIGINT UNSIGNED NOT NULL COMMENT '分片键(≡uid≡user_id≡tenant_id)',
  `product_code`  VARCHAR(32) NOT NULL DEFAULT '' COMMENT '限定抵扣的产品;空表示通用包',
  `sku_code`      VARCHAR(64) NOT NULL COMMENT '资源包SKU;如 euecs.pack.1000cpu.hour',
  `face_value`    DECIMAL(18,6) NOT NULL COMMENT '购买额度(面值);micro-units,禁浮点(03§6)',
  `remaining`     DECIMAL(18,6) NOT NULL COMMENT '剩余额度;∈[0,face_value],终态为0',
  `purchased_at`  DATETIME NOT NULL COMMENT '购买订单结算时间;额度到账时刻',
  `expire_at`     DATETIME DEFAULT NULL COMMENT '额度到期;NULL表示永不过期',
  `status`        TINYINT NOT NULL DEFAULT 1 COMMENT '1ACTIVE 2EXHAUSTED 3EXPIRED',
  `version`       INT NOT NULL DEFAULT 0 COMMENT '乐观锁;consume/refund/expire带WHERE version=?',
  `created_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`pack_id`),
  UNIQUE KEY `uk_pack` (`account_id`,`pack_id`) COMMENT '幂等;购买重放只到账一次',
  KEY `idx_acc_status_expire` (`account_id`,`status`,`expire_at`) COMMENT '到期回收扫描;ACTIVE且expire_at已过'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='资源包额度账本;remaining∈[0,face],到期 forfeit 残余额度(D6/M-4.1)';

-- -----------------------------------------------------------------------------
-- resource_pack_journal — 资源包流水(append-only)
--
-- One immutable row per quota movement. uk_idem makes consume/refund/expire
-- exactly-once: a retried settlement re-uses the same idempotency key and is a
-- no-op, so a retry cannot double-spend quota. This mirrors ledger.entry's
-- idempotency contract (03§8.5) — the money domain's shared shape.
--
-- balance_after is the remaining after this entry, denormalised so the journal
-- alone reconstructs the balance timeline without joining resource_pack.
-- -----------------------------------------------------------------------------
CREATE TABLE `resource_pack_journal` (
  `entry_id`     BIGINT UNSIGNED NOT NULL,
  `pack_id`      VARCHAR(64) NOT NULL,
  `account_id`   BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键;禁止跨库 JOIN 故冗余',
  `entry_type`   TINYINT NOT NULL COMMENT '1PURCHASE 2CONSUME 3REFUND 4EXPIRE',
  `amount`       DECIMAL(18,6) NOT NULL COMMENT '本次变动额;consume/expire为正扣减,refund为正增加',
  `balance_after` DECIMAL(18,6) NOT NULL COMMENT '本次变动后余额;流水自洽无需关联主表',
  `biz_key`      VARCHAR(64) NOT NULL DEFAULT '' COMMENT '关联业务键;consume=charge_id,purchase=order_id',
  `idempotency_key` VARCHAR(96) NOT NULL COMMENT '幂等键;重放同一key为no-op,防双扣',
  `created_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`entry_id`),
  UNIQUE KEY `uk_idem` (`account_id`,`pack_id`,`idempotency_key`) COMMENT '幂等;同一key只生效一次',
  KEY `idx_pack_time` (`account_id`,`pack_id`,`created_at`) COMMENT '按时间倒序查流水'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='资源包流水(append-only);幂等键防双扣,镜像ledger.entry契约(03§8.5)';
