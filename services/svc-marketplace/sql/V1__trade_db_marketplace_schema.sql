-- =============================================================================
-- svc-marketplace DDL — trade_db V1 (云市场, 三期 M-10)
--
-- 云市场是平台的三方商品交易面 (09§5.1 goal 3 / §5.2 M-10): 合作伙伴(ISV)上架
-- 镜像/SaaS/服务, 平台审核, 客户购买, 每单在伙伴与平台间分账 (pkg-go/settlement).
--
-- 分片键: account_id 单键 (裁决 C5/S12, 与全平台一致) —— listing 的 owner 是
-- partner_id (即第三方 ISV 的 account_id), 结算记录按 partner_id 聚合对账.
-- 金额一律 DECIMAL(18,6) micro-units (pricing.Amount), 绝不 float.
--
-- 履约口径 (06 章): 三方产品按"回调合作伙伴自身 OpenAPI"履约, 平台不为其建
-- rc-* 控制器 —— listing.openapi_url 就是履约入口, 平台只做商品面 + 交易面.
--
-- Source-only: 不在本机执行 (无 MySQL). DDL 完整、内部自洽、静态可校验.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- marketplace_listing — 商品上架 (上架审核生命周期)
-- -----------------------------------------------------------------------------
CREATE TABLE `marketplace_listing` (
  `listing_id`        BIGINT       NOT NULL COMMENT '商品ID(全局自增,服务内序列)',
  `partner_id`        BIGINT       NOT NULL COMMENT '第三方ISV账号(分账对象,account_id分片)',
  `name`              VARCHAR(128) NOT NULL COMMENT '商品名',
  `category`          VARCHAR(16)  NOT NULL COMMENT 'IMAGE/SAAS/SERVICE(01§3.2 三类)',
  `openapi_url`       VARCHAR(512) NOT NULL COMMENT '履约回调:合作伙伴自身OpenAPI入口(06章口径)',
  `status`            VARCHAR(24)  NOT NULL DEFAULT 'DRAFT' COMMENT 'DRAFT/PENDING_APPROVAL/APPROVED/REJECTED/OFF_SHELF',
  `partner_rate_bps`  INT          NOT NULL DEFAULT 0 COMMENT '分账比例(基点0..10000,见pkg-go/settlement.Rate)',
  `created_at`        DATETIME(3)  NOT NULL,
  `updated_at`        DATETIME(3)  NOT NULL,
  PRIMARY KEY (`listing_id`),
  KEY `idx_partner_status` (`partner_id`, `status`) COMMENT '伙伴维度:我的上架审核状态',
  KEY `idx_status_category` (`status`, `category`) COMMENT '对客目录:已上架按类目过滤'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='云市场商品上架(上架审核,09§5.2 M-10)';

-- -----------------------------------------------------------------------------
-- marketplace_settlement — 分账结算记录 (对账不可变)
--
-- partner_micro + platform_micro 必须恒等于 gross_micro (无未解释差异, 03§4.2.4),
-- 由 pkg-go/settlement.Settle 保证 (platform = gross - partner 精确派生).
-- uk_order 保证同一订单只结算一次 (幂等, 防双付).
-- -----------------------------------------------------------------------------
CREATE TABLE `marketplace_settlement` (
  `settlement_id`    BIGINT        NOT NULL COMMENT '结算ID',
  `order_id`         VARCHAR(64)   NOT NULL COMMENT '订单ID(幂等键)',
  `listing_id`       BIGINT        NOT NULL COMMENT '被结算商品',
  `partner_id`       BIGINT        NOT NULL COMMENT '分账对象(account_id分片)',
  `gross_micro`      DECIMAL(18,6) NOT NULL COMMENT '订单总额(micro-units)',
  `partner_rate_bps` INT           NOT NULL COMMENT '伙伴分账比例(基点)',
  `partner_micro`    DECIMAL(18,6) NOT NULL COMMENT '伙伴份额(micro-units)',
  `platform_micro`   DECIMAL(18,6) NOT NULL COMMENT '平台份额(micro-units)',
  `settled_at`       DATETIME(3)   NOT NULL,
  PRIMARY KEY (`settlement_id`),
  UNIQUE KEY `uk_order` (`order_id`) COMMENT '幂等:同一订单只结算一次(防双付)',
  KEY `idx_partner` (`partner_id`, `settled_at`) COMMENT '伙伴对账:按月聚合份额'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='云市场分账结算(partner+platform≡gross,不可变)';
