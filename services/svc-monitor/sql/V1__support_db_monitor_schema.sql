-- =============================================================================
-- svc-monitor DDL — support_db (监控告警) + alert-engine 共用 alert_rule
--
-- 规则归属 (03§4.4.1/§4.4.5): 租户告警规则 CRUD 归 svc-monitor(只做规则管理与
-- 查询代理,不做评估);规则评估由 alert-engine(租户规则引擎)每分钟从租户
-- VictoriaMetrics 集群拉取指标执行. 二者共享 MySQL alert_rule —— svc-monitor
-- 写规则,alert-engine 只读评估,规则变更经同步通知 alert-engine 生效.
--
-- Sharding: account_id single key (04§6.3). Every tenant-facing table carries
-- account_id; alert_rule_template is platform product-level metadata and, like
-- api-action, is a global broadcast set with no account_id and must never be
-- JOINed against a sharded tenant table.
--
-- 租户侧不部署 Prometheus/AlertManager (04§4.4.1 口径,以 05§8/§9 为准): 规则
-- 落 MySQL,不存在"把规则编译为 AlertManager 配置"的路径.
--
-- Source of truth: 03-backend-services.md §4.4.1 and §4.4.5; 04§6.3.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- alert_rule — 租户告警规则 (svc-monitor 写 / alert-engine 读)
--
-- uk_acc_rule makes PutMetricRule idempotent: the same account+product+metric+
-- resource_type may hold only one rule, so a retry upserts instead of spawning
-- a second rule that would double-fire alerts.
--
-- version drives the optimistic lock, and is the same field alert-engine reads
-- to detect rule changes: a rule edited between evaluations must not be
-- evaluated half-old/half-new.
--
-- Rules are per-account, so account_id is the shard key. alert-engine reads
-- across shards to evaluate every rule; that read is a scan by status, not a
-- cross-shard join, which the sharding rules allow.
-- -----------------------------------------------------------------------------
CREATE TABLE `alert_rule` (
  `rule_id`                  BIGINT UNSIGNED NOT NULL,
  `account_id`               BIGINT UNSIGNED NOT NULL COMMENT '分片键(≡uid≡user_id≡tenant_id)',
  `product_code`             VARCHAR(32) NOT NULL COMMENT '产品代号: scecs 等',
  `resource_type`            VARCHAR(32) NOT NULL COMMENT '资源类型: instance 等',
  `metric`                   VARCHAR(64) NOT NULL COMMENT '指标: cpu_utilization 等',
  `threshold`                DECIMAL(18,4) NOT NULL COMMENT '阈值;评估值≥/≤阈值触发',
  `comparison_operator`      TINYINT NOT NULL DEFAULT 1 COMMENT '1≥ 2> 3≤ 4< 5==',
  `period`                   INT NOT NULL DEFAULT 60 COMMENT '采样周期(秒)',
  `eval_periods`             INT NOT NULL DEFAULT 1 COMMENT '连续 N 个周期满足才触发;防抖',
  `notification_channels_json` JSON NOT NULL COMMENT '通知渠道: ["IN_APP","SMS","EMAIL","WEBHOOK"]',
  `status`                   TINYINT NOT NULL DEFAULT 1 COMMENT '1启用 2停用;停用后 alert-engine 跳过',
  `version`                  INT NOT NULL DEFAULT 0 COMMENT '乐观锁;编辑带 WHERE version=?',
  `created_at`               DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`               DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`rule_id`),
  UNIQUE KEY `uk_acc_rule` (`account_id`,`product_code`,`resource_type`,`metric`) COMMENT '幂等;同账户同指标只一规则,不重复告警',
  KEY `idx_account` (`account_id`,`created_at`),
  KEY `idx_status_scan` (`status`,`updated_at`) COMMENT 'alert-engine 评估扫描;按 status 全量扫描,非跨片 JOIN'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='租户告警规则;svc-monitor 写、alert-engine 评估(03§4.4.1/§4.4.5)';

-- -----------------------------------------------------------------------------
-- alert_rule_template — 平台预置规则模板 (全局广播表,无分片键)
--
-- DescribeMetricRuleTemplates(product_code) reads these (03§4.4.1). Product-level
-- metadata: no account_id, no sharding, and it must never be JOINed against the
-- sharded alert_rule. A tenant "using a template" copies template_json into an
-- alert_rule row on their own shard.
-- -----------------------------------------------------------------------------
CREATE TABLE `alert_rule_template` (
  `template_id`  BIGINT UNSIGNED NOT NULL,
  `product_code` VARCHAR(32) NOT NULL,
  `template_json` JSON NOT NULL COMMENT '预置规则模板: 指标/阈值/周期/渠道;租户采纳时拷入 alert_rule',
  `created_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`template_id`),
  UNIQUE KEY `uk_product_template` (`product_code`,`template_id`),
  KEY `idx_product` (`product_code`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='平台预置规则模板;无分片键的全局广播表(03§4.4.1)';
