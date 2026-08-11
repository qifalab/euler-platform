-- =============================================================================
-- svc-org DDL — account_db (组织与项目)
--
-- Sharding: account_id single key, 4 库 × 16 表 (04§6.3). Every table below
-- carries account_id, including child tables where it is redundant — a
-- redundant shard key is cheaper than a cross-shard join, which the sharding
-- rules forbid outright (03 附录A: 禁止跨库 JOIN).
--
-- 职责 (03§4.1.2): 资源目录(企业多账号后置)、项目(资源组)CRUD、资源归属与
-- 移动、统一标签(Tag)体系. 标签是成本分析与权限的公共语言,模型第一天就要稳.
-- 项目下资源计数经 Kafka 消费 cloud.resource.lifecycle.event 维护(04§5.4),
-- 本库不跨库 JOIN resource_instance.
--
-- Source of truth: 03-backend-services.md §4.1.2; 04§6.3.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- org_project — 项目(资源组)
--
-- parent_id supports a two-level project tree (account → project → sub-project).
-- The single shard key is account_id; a parent may live in the same shard, but
-- tree traversal deliberately avoids a cross-shard join by constraining the
-- tree depth and resolving ancestry through the same-shard rows or service
-- reads, never an arbitrary-depth recursive cross-shard join.
--
-- Unique keys give idempotency: CreateProject retries cannot create a second
-- project under the same account+name, which is what makes MoveResource /
-- TagResources safe to retry against this schema.
-- -----------------------------------------------------------------------------
CREATE TABLE `org_project` (
  `project_id`  BIGINT UNSIGNED NOT NULL COMMENT '项目ID;号段服务发号,内嵌分片因子(04§6.6)',
  `account_id`  BIGINT UNSIGNED NOT NULL COMMENT '分片键(≡uid≡user_id≡tenant_id)',
  `name`        VARCHAR(64) NOT NULL COMMENT '项目名,同账号内唯一',
  `parent_id`   BIGINT UNSIGNED DEFAULT NULL COMMENT '父项目ID;顶层为 NULL,树深受限,避免跨分片任意深度 JOIN',
  `status`      TINYINT NOT NULL DEFAULT 1 COMMENT '1启用 2停用 3删除;停用后资源不可迁移入',
  `version`     INT NOT NULL DEFAULT 0 COMMENT '乐观锁;UpdateProject 带 WHERE version=?',
  `created_at`  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`project_id`),
  UNIQUE KEY `uk_acc_name` (`account_id`,`name`) COMMENT '幂等;同一账号下项目名唯一,重试不重复建',
  KEY `idx_acc_parent` (`account_id`,`parent_id`),
  KEY `idx_acc_created` (`account_id`,`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='项目(资源组);资源归属与移动的容器(03§4.1.2)';

-- -----------------------------------------------------------------------------
-- org_project_resource — 项目↔资源映射
--
-- resource_id lives in resource_db, so the mapping is resolved through the
-- services (svc-org/svc-orchestrator), never by a cross-database JOIN. The
-- redundant account_id shard key keeps every row on the owning account's shard,
-- so "list my account's projects" and "which project owns this resource" both
-- stay single-shard queries.
-- -----------------------------------------------------------------------------
CREATE TABLE `org_project_resource` (
  `id`         BIGINT UNSIGNED NOT NULL,
  `resource_id` VARCHAR(64) NOT NULL COMMENT '资源ID;跨库,经服务解析不直接 JOIN',
  `project_id` BIGINT UNSIGNED NOT NULL,
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键;禁止跨库 JOIN 故冗余而非关联',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_resource` (`resource_id`) COMMENT '一个资源同一时刻只属一个项目;幂等',
  KEY `idx_project` (`account_id`,`project_id`) COMMENT '项目下资源列表主查询',
  KEY `idx_acc_created` (`account_id`,`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='项目资源映射;资源归属与移动(MoveResource)的落点(03§4.1.2)';

-- -----------------------------------------------------------------------------
-- org_tag — 标签定义
--
-- Tags are the common language of cost analysis and permissions (03§4.1.2).
-- (account_id, key) unique makes TagResources idempotent at the key level; a
-- duplicate tag on the same account is a retry, not a new tag.
-- -----------------------------------------------------------------------------
CREATE TABLE `org_tag` (
  `tag_id`     BIGINT UNSIGNED NOT NULL,
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `tag_key`    VARCHAR(64) NOT NULL COMMENT '标签键;同账号内唯一',
  `tag_value`  VARCHAR(128) NOT NULL,
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`tag_id`),
  UNIQUE KEY `uk_acc_key` (`account_id`,`tag_key`) COMMENT '幂等;同账号同键只一个标签',
  KEY `idx_acc_value` (`account_id`,`tag_value`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='标签定义(03§4.1.2)';

-- -----------------------------------------------------------------------------
-- org_tag_resource — 资源↔标签映射
--
-- resource_id is cross-database (resource_db); resolved via service, never a
-- cross-db JOIN. The redundant account_id shard key keeps tag lookups
-- single-shard, and the (resource_id, tag_key) unique pair makes UntagResources
-- idempotent.
-- -----------------------------------------------------------------------------
CREATE TABLE `org_tag_resource` (
  `id`         BIGINT UNSIGNED NOT NULL,
  `resource_id` VARCHAR(64) NOT NULL COMMENT '资源ID;跨库,经服务解析',
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '冗余分片键',
  `tag_key`    VARCHAR(64) NOT NULL,
  `tag_value`  VARCHAR(128) NOT NULL COMMENT '冗余值快照;按值过滤无需 JOIN org_tag',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_res_key` (`resource_id`,`tag_key`) COMMENT '一资源同键只一标签;UntagResources 幂等',
  KEY `idx_acc_tag` (`account_id`,`tag_key`,`tag_value`) COMMENT '按标签筛选资源主查询(成本/权限公共语言)',
  KEY `idx_acc_created` (`account_id`,`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='资源标签映射;TagResources/UntagResources 落点(03§4.1.2)';
