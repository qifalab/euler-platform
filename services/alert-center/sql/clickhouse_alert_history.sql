-- =============================================================================
-- alert-center DDL — ClickHouse 告警历史 (03§4.4.6, 三期 D-1 云监控高级告警)
--
-- alert-center 的告警历史入 ClickHouse (03§4.4.6), 与 svc-monitor 的 MySQL
-- alert_rule (规则管理) 分层: 规则在 MySQL (低频 CRUD), 告警历史在 ClickHouse
-- (高频写入 + 时序检索 + 长保留). 收敛后的告警写此表, 控制台按身份/资源/时间
-- 查询 (DescribeAlerts).
--
-- 保留期: 平台合规基线 ≥180 天热存 (等保三级) + MinIO 冷备; 付费档 365 天/18 个月
-- (03§4.4.3 权威, 三章同步). ClickHouse TTL 与审计/日志同构 (05§11).
--
-- Source-only: 不在本机执行 (无 ClickHouse). DDL 完整、内部自洽、静态可校验.
-- =============================================================================

CREATE TABLE IF NOT EXISTS alert_center.alert_history
(
    `event_time`     DateTime64(3, 'UTC') NOT NULL COMMENT '告警发生时间',
    `alert_id`       String                NOT NULL COMMENT '收敛后告警ID',
    `tenant_id`      Int64                 NOT NULL COMMENT '租户(account_id 分片维度)',
    `product`        LowCardinality(String) COMMENT '产品 sc 前缀',
    `metric`         String                COMMENT '指标名',
    `severity`       LowCardinality(String) COMMENT 'CRITICAL/WARNING/INFO',
    `group_key`      String                COMMENT '分组键 tenant|product',
    `dedup_count`    UInt32                DEFAULT 1 COMMENT '去重合并的原始告警数',
    `notified_via`   Array(String)         COMMENT '实际通知通道(经 cloud.notify.message)',
    `labels`         Map(String, String)   COMMENT '资源标签'
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(event_time)
ORDER BY (tenant_id, product, event_time)
TTL toDateTime(event_time) + INTERVAL 180 DAY
COMMENT '收敛后告警历史(合规基线180天热存+MinIO冷备,付费档365天/18个月)';
