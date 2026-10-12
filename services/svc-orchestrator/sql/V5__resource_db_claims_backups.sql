-- =============================================================================
-- resource_db V5 — 资源全局唯一名 + 备份快照
--
-- resource_name_claim — 跨账号唯一的资源名(EUOSS 桶名:同一地域内全平台唯一)。
--   resource_instance_attr 随 account_id 分片,表达不了"跨账号唯一";这张表以
--   (claim_ns, region, name) 为主键,由数据库裁决两个账号同时抢同一个桶名:
--   输的一方插入撞键,履约返回 409 Resource.BucketExists,资源行与属性随事务
--   一起回滚。资源进入 CREATE_FAILED(已回滚)或 RELEASED 时,在同一事务里删除
--   它的占用 —— 名字随资源释放,可被再次使用。
--   分片口径:按 (claim_ns, region, name) 路由的全局表,不随 account_id 分片
--   (与 uk 语义一致:唯一性本身就是跨租户的)。
--
-- resource_backup — EUBACKUP 快照记录(POST/GET/DELETE
--   /api/v1/orchestrator/resources/{id}/backups)。expires_at 在快照生成时由
--   pkg-go/backup.RetainUntil(created_at, retentionDays) 定死:之后缩短保留期
--   不会追溯删除已按旧承诺保留的快照。过期(now > expires_at)的快照在列表时
--   被清理(06§4.1:保留期必须被执行,存储不得无界增长)。
--   backup_id = bak-{毫秒时间戳 12 位 hex}{随机 8 位 hex}:按时间可排序,
--   同一秒内的快照也能按"最新在前"稳定排序。
--
-- 可重入:CREATE TABLE IF NOT EXISTS。
-- =============================================================================

CREATE TABLE IF NOT EXISTS `resource_name_claim` (
  `claim_ns`    VARCHAR(32) NOT NULL COMMENT '命名空间:euoss.bucket',
  `region`      VARCHAR(32) NOT NULL,
  `name`        VARCHAR(255) NOT NULL,
  `resource_id` VARCHAR(64) NOT NULL COMMENT '占用该名字的资源',
  `account_id`  BIGINT UNSIGNED NOT NULL,
  `created_at`  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`claim_ns`,`region`,`name`) COMMENT '同一地域同一命名空间全平台唯一',
  KEY `idx_resource` (`resource_id`) COMMENT '资源回滚/释放时删除其占用'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='跨账号唯一资源名(桶名等);随资源回滚/释放删除';

CREATE TABLE IF NOT EXISTS `resource_backup` (
  `backup_id`   VARCHAR(64) NOT NULL COMMENT 'bak-{毫秒时间戳12位hex}{随机8位hex}',
  `resource_id` VARCHAR(64) NOT NULL COMMENT 'EUBACKUP 资源',
  `account_id`  BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `status`      VARCHAR(16) NOT NULL DEFAULT 'COMPLETED',
  `size_gb`     INT UNSIGNED DEFAULT NULL COMMENT '未知为 NULL',
  `created_at`  DATETIME NOT NULL,
  `expires_at`  DATETIME NOT NULL COMMENT 'created_at + retentionDays(backup.RetainUntil);此刻之后过期并清理',
  PRIMARY KEY (`backup_id`),
  KEY `idx_resource_created` (`resource_id`,`created_at`) COMMENT '快照列表:最新在前',
  KEY `idx_expires` (`expires_at`) COMMENT '过期清理扫描'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='资源备份快照;保留期到期即清理';
