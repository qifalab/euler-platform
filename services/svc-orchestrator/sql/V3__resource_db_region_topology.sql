-- =============================================================================
-- svc-orchestrator DDL — resource_db V3 (两地三中心, 三期 M-8)
--
-- V1 已建 resource_instance(region 列为 Day1 预留字段, 00§1.2 P3 原则);
-- V2(M-6) 把 az_id 实装为 AZ 级故障域维度. 本迁移(V3, M-8)把 region 从
-- "预留元数据"实装为"地域级故障域维度": 加跨地域复制水位表(RPO 观测) +
-- 地域级影响面视图(双地域容灾演练核心查询).
--
-- 与 V2 同一纪律: 不动分片键(account_id 单键, 裁决 C5/S12), region 是元数据
-- 维度不进 shard key; 资源 ID 规范仍由 pkg-go/identifier 单源定义. 跨地域
-- 语义(全局 vs 地域服务、复制通道 RPO、冷备接管)由 pkg-go/multiregion 单源
-- 定义, 本 DDL 是其数据面载体.
--
-- 00§4.5 P3 要点: 账务 binlog 准实时(RPO≈0, canal/otter 类可选)+ MinIO
-- Replication 异步; 灾备接管 = 管控面冷转热 + DNS 切换(RTO ≤ 30min);
-- Kafka 不做跨城镜像(异地按 cloud.* 规范重建 topic).
--
-- Source-only: 不在本机执行 (无 MySQL). DDL 完整、内部自洽、静态可校验.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- region_replication_status — 跨地域复制水位 (M-8, 00§4.5)
--
-- 三期 C1(09§5.3)要求"核心链路地域级故障切换演练通过"; RPO 的实测口径是
-- 复制水位 = now - last_replicated_at. 本表是各复制通道(RPO 目标见
-- pkg-go/multiregion.CoreLedgerChannel / ObjectStorageChannel)的水位台账,
-- 让 SLO 燃尽看板(三期 M-9)和容灾演练(M-11)从同一条数据源读 RPO, 而非
-- 各通道自报口径. ledger-binlog 通道 RPO≈0(核心账务), object-storage 通道
-- 异步(小时级).
-- -----------------------------------------------------------------------------
CREATE TABLE `region_replication_status` (
  `region`             VARCHAR(32)  NOT NULL COMMENT '目标地域(cn-east-1,identifier.IsValidRegion)',
  `channel`            VARCHAR(32)  NOT NULL COMMENT '复制通道:ledger-binlog/object-storage(00§4.5)',
  `last_replicated_at` DATETIME(3)  NOT NULL COMMENT '最近一次复制成功时间;RPO观测=now-该值',
  `lag_ms`             BIGINT       NOT NULL COMMENT '实测复制延迟(毫秒),供RPO燃尽告警',
  `updated_at`         DATETIME(3)  NOT NULL,
  PRIMARY KEY (`region`, `channel`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='跨地域复制水位(RPO≈0核心账务/00§4.5,M-8)';

-- -----------------------------------------------------------------------------
-- v_resource_by_region — 地域级资源分布视图 (M-8 双地域演练用)
--
-- 与 V2 的 v_resource_by_az 同族, 但聚合维度是 region 而非 az: 双地域容灾
-- 演练(M-11)摘除整个主地域时, SRE 需要立刻知道该地域有多少 RUNNING 资源、
-- 分属哪些租户和产品 —— 这是"地域级故障影响面"的台账起点.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE VIEW `v_resource_by_region` AS
SELECT
  r.region,
  r.product_code,
  r.status,
  COUNT(*)                    AS resource_count,
  COUNT(DISTINCT r.account_id) AS tenant_count
FROM resource_instance r
GROUP BY r.region, r.product_code, r.status;

-- -----------------------------------------------------------------------------
-- v_resource_region_failover_impact — 单地域摘除影响面 (M-11 runbook 核心查询)
--
-- 给定一个 region, 列出该地域全部活跃资源及所属租户/产品/订单. 容灾演练时
-- 这条视图回答"如果现在整个地域不可用, 谁的什么资源会断". 与 V2 的
-- v_resource_az_failover_impact 分层: az 级是"同城双 AZ 摘一侧", 地域级是
-- "主地域整体失效, 异地冷备接管" —— 后者正是 P3 的承诺(00§4.5).
-- -----------------------------------------------------------------------------
CREATE OR REPLACE VIEW `v_resource_region_failover_impact` AS
SELECT
  r.resource_id,
  r.account_id,
  r.product_code,
  r.resource_type,
  r.region,
  r.az_id,
  r.status,
  r.charge_type,
  r.order_id,
  r.billing_start
FROM resource_instance r
WHERE r.status IN ('RUNNING','UPGRADING','CREATING','PREEMPTING');
