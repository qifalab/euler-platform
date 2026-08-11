-- =============================================================================
-- svc-api-meta DDL — openapi_meta (API 元数据与 SDK)
--
-- 职责 (03§4.5.1): API 元数据中心 —— 存储每个 Action 的参数 schema、错误码、
-- 版本;驱动文档自动生成与 SDK 生成;提供 OpenAPI Explorer 调试台后端.
--
-- This database is product-level metadata, NOT tenant data: it has no
-- account_id shard key. It is a global broadcast table set (read-mostly,
-- metadata for the whole platform), in the style of quota_definition. It does
-- not participate in account_id sharding and must never be JOINed against a
-- sharded tenant table.
--
-- 兼容性 (03§4.5.1): APISIX 依赖 etcd;路由按 product_code 到产品控制面服务,
-- 本库存储的 schema/错误码即路由与校验的事实源.
--
-- Source of truth: 03-backend-services.md §4.5.1.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- api_action — 每个产品 Action 的元数据
--
-- param_schema_json / error_codes_json are JSON so the action contract evolves
-- without DDL churn (read-mostly, validated at write time). version + status
-- drive contract evolution: an API version is immutable once published, and
-- status gates whether the OpenAPI gateway accepts traffic for it.
-- -----------------------------------------------------------------------------
CREATE TABLE `api_action` (
  `action_id`        BIGINT UNSIGNED NOT NULL,
  `product_code`     VARCHAR(32) NOT NULL COMMENT '产品代号: scecs 等(01§1.1 D0)',
  `action_name`      VARCHAR(64) NOT NULL COMMENT 'Action: DescribeInstances 等',
  `param_schema_json` JSON NOT NULL COMMENT '参数 schema;驱动 SDK 生成与网关校验',
  `error_codes_json` JSON NOT NULL COMMENT '错误码字典;驱动文档与错误映射',
  `version`          INT NOT NULL DEFAULT 1 COMMENT 'Action 版本;发布后不可变',
  `status`           TINYINT NOT NULL DEFAULT 1 COMMENT '1已发布 2草稿 3已下线;下线后网关拒流量',
  `created_at`       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`action_id`),
  UNIQUE KEY `uk_product_action_version` (`product_code`,`action_name`,`version`) COMMENT '幂等;同产品同 Action 同版本唯一',
  KEY `idx_product_status` (`product_code`,`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='产品 Action 元数据;schema/错误码/版本驱动文档与 SDK(03§4.5.1)';

-- -----------------------------------------------------------------------------
-- api_doc — 文档内容(自动生成落点)
--
-- One doc per (product_code, version). uk_product_version makes doc generation
-- idempotent: re-running the doc generator upserts instead of duplicating.
-- content_md is the rendered Markdown pushed by svc-api-meta from api_action,
-- kept here so the doc center reads one table rather than assembling on the fly.
-- -----------------------------------------------------------------------------
CREATE TABLE `api_doc` (
  `doc_id`       BIGINT UNSIGNED NOT NULL,
  `product_code` VARCHAR(32) NOT NULL,
  `version`      INT NOT NULL DEFAULT 1 COMMENT '对应 api_action.version',
  `content_md`   MEDIUMTEXT NOT NULL COMMENT 'Markdown 文档正文;由元数据自动生成',
  `created_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`doc_id`),
  UNIQUE KEY `uk_product_version` (`product_code`,`version`) COMMENT '幂等;文档生成重跑不重复',
  KEY `idx_product` (`product_code`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='API 文档;由元数据自动生成的落点(03§4.5.1)';
