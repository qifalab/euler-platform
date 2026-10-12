-- =============================================================================
-- resource_db V4 — 一个订单只产出一个资源 (uk_order)
--
-- 履约幂等原先只靠"先 ByOrder 查、再插入":两个副本同时履约同一订单时,两边都
-- 查不到,各自插入一行 —— idx_order 是普通索引,资源 ID 又带进程内计数器,于是
-- 一笔付款开出两台实例。svc-orchestrator 现在由订单确定性地派生 resource_id,
-- 本迁移再把 order_id 升为唯一键,让数据库裁决这场竞争:输的一方插入撞键,
-- 回读赢家的资源返回。
--
-- order_id 可空(种子/非订单资源),UNIQUE 允许多个 NULL。订单单行(一单一 SKU),
-- 故唯一键是 order_id 本身而非 (order_id, line)。按 account_id 分片时同一订单
-- 总落在同一分片,分片内唯一即全局唯一。
--
-- 已有重复数据时 ADD UNIQUE 会失败(1062),须先人工处理:
--   SELECT order_id, COUNT(*) FROM resource_instance
--    WHERE order_id IS NOT NULL GROUP BY order_id HAVING COUNT(*) > 1;
--
-- 可重入:MySQL 的 ALTER TABLE 没有 ADD/DROP INDEX IF [NOT] EXISTS,
-- 用 information_schema 判断后动态执行。
-- =============================================================================

SET @uk_order_exists := (
  SELECT COUNT(*) FROM information_schema.statistics
   WHERE table_schema = DATABASE() AND table_name = 'resource_instance' AND index_name = 'uk_order');
SET @ddl := IF(@uk_order_exists = 0,
  'ALTER TABLE `resource_instance` ADD UNIQUE KEY `uk_order` (`order_id`) COMMENT ''一个订单只产出一个资源;履约竞争由此裁决''',
  'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

-- idx_order 被 uk_order 完全覆盖,删掉免得每次写入多维护一棵索引树。
SET @idx_order_exists := (
  SELECT COUNT(*) FROM information_schema.statistics
   WHERE table_schema = DATABASE() AND table_name = 'resource_instance' AND index_name = 'idx_order');
SET @ddl := IF(@idx_order_exists > 0,
  'ALTER TABLE `resource_instance` DROP INDEX `idx_order`',
  'DO 0');
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
