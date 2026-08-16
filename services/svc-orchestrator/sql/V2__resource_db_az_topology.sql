-- =============================================================================
-- svc-orchestrator DDL — resource_db V2 (同城双 AZ, 二期 M-6)
--
-- 一期 V1 已建 resource_instance(zone 列预留) + resource_state_log +
-- provision_task + outbox + 流程引擎. 本迁移把"预留的 AZ 字段"实装为可查询的
-- 故障域维度: az_id 正式化、加 AZ 级扫描索引、补一张 AZ 拓扑视图.
--
-- 不动分片键 (account_id 单键, 裁决 C5/S12 否决 region+account_id 复合路由),
-- az_id 是资源元数据维度, 不进 shard key. 资源 ID 规范仍由 pkg-go/identifier
-- 单源定义 ({productCode}-{regionId}-{分片因子}-{随机}); az_id 不内嵌进 resource_id
-- (分片因子内嵌是 04§6.6 的反查优化, az 不是路由维度).
--
-- 00§4.4 P2 要点 2: MySQL MGR 单主跨 AZ; 要点 3: 副本跨 AZ 分布客户端容忍单
-- AZ 全失. az_id 列让"AZ 级故障切换演练"能从台账直接定位受影响资源 —— 否则演练
-- 只能靠 K8s 标签, 台账与集群视图脱节.
--
-- Source-only: 不在本机执行 (无 MySQL). 与一期口径一致: DDL 完整、内部自洽、
-- 静态可校验 (tools/check-yaml-syntax.py 不覆盖 .sql; 结构由命名/注释不变式自证).
-- =============================================================================

-- -----------------------------------------------------------------------------
-- resource_instance: az_id 正式化
--
-- 一期 zone VARCHAR(32) 是 Day1 预留字段 (00§1.2 P3). M-6 把它从"预留"变为
-- "实装": 重命名为 az_id 语义, 加索引. REGIONAL 资源 (scoss/scvpc/scmon) 的
-- az_id 可空 (它们跨 AZ, 无单一归宿); ZONAL 资源 (scecs/scrds/scbs/sceip) 的
-- az_id 非空 (绑定到一个 AZ, 见 svc-catalog RegionScope M-6.1b).
--
-- 不用 ALTER TABLE RENAME COLUMN 是为兼容 5.7 (rename column 是 8.0+); 这里用
-- ADD COLUMN + 数据迁移注释 + 保留旧列的方式 (与一期其它迁移的兼容口径一致).
-- 生产环境执行时按注释迁移历史数据; 源码口径下本 DDL 描述目标态.
-- -----------------------------------------------------------------------------
ALTER TABLE `resource_instance`
  ADD COLUMN `az_id` VARCHAR(32) DEFAULT NULL
    COMMENT '可用区ID(cn-north-1-a,见identifier.AZName);REGIONAL资源可空,ZONAL资源非空(M-6,00§4.1)'
  AFTER `zone`,
  ADD KEY `idx_region_az` (`region`,`az_id`,`status`)
    COMMENT 'AZ级故障切换演练:按region+az定位受影响资源(M-6.3 runbook)',
  ADD KEY `idx_az_status` (`az_id`,`status`)
    COMMENT '单AZ摘除影响面扫描:az_id+status快速圈定RUNNING资源';

-- -----------------------------------------------------------------------------
-- resource_instance.zone 冗余标注 (向后兼容)
--
-- zone 与 az_id 在 M-6 后语义重叠. 保留 zone 列避免破坏一期已依赖它的查询
-- (一期它是 NULL 占位, 无实际写入方, 故保留无数据迁移风险). 新代码用 az_id.
-- -----------------------------------------------------------------------------
ALTER TABLE `resource_instance`
  MODIFY COLUMN `zone` VARCHAR(32) DEFAULT NULL
    COMMENT '已弃用,等价 az_id;保留向后兼容,新代码用 az_id(M-6)';

-- -----------------------------------------------------------------------------
-- v_resource_by_az — AZ 级资源分布视图 (M-6 故障切换演练用)
--
-- 演练 AZ-A 摘除时, SRE 需要立刻知道该 AZ 有多少 RUNNING 资源、分属哪些租户
-- 和产品. 这个视图把台账按 az 聚合, 让演练和事后核对都从一条 SQL 开始, 而非
-- 现场拼查询. 与一期 v_resource_stuck_intermediate / v_resource_billing_exceptions
-- 同属台账对账视图族.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE VIEW `v_resource_by_az` AS
SELECT
  r.region,
  r.az_id,
  r.product_code,
  r.status,
  COUNT(*)                 AS resource_count,
  COUNT(DISTINCT r.account_id) AS tenant_count
FROM resource_instance r
WHERE r.az_id IS NOT NULL
GROUP BY r.region, r.az_id, r.product_code, r.status;

-- -----------------------------------------------------------------------------
-- v_resource_az_failover_impact — 单 AZ 摘除影响面 (M-6.3 runbook 核心查询)
--
-- 给定一个 az_id, 列出该 AZ 全部 RUNNING 资源及其所属租户/产品/订单. 演练时
-- 这条视图就是"如果现在摘掉这个 AZ, 谁的什么资源会断". 00§4.4 要点 1: 管控面
-- 无状态服务双 AZ 对等摘除; 要点 2: MySQL MGR 单主跨 AZ 故障切换由管控编排.
-- 有状态 ZONAL 资源的跨 AZ 冗余由 CrossAZReplicas (M-6.1b) 决定: HA 规格的
-- ZONAL 资源在 AZ 摘除后由副本接管, 非 HA 规格则进降级/待恢复清单.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE VIEW `v_resource_az_failover_impact` AS
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
WHERE r.az_id IS NOT NULL
  AND r.status IN ('RUNNING','UPGRADING','CREATING','PREEMPTING');
