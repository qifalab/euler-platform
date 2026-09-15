-- =============================================================================
-- trade_db V2 — svc-order 落库缺口补齐
--
-- 1. 三处代理主键补 AUTO_INCREMENT
--    order_state_log.id / outbox_message.id / idempotent_record.id 都是纯代理键:
--    业务身份是 (order_id, occurred_at)、uk_event_id、uk_biz(biz_type,biz_key),
--    领域结构里没有这三个 id 字段。没有 AUTO_INCREMENT 就没有任何写入方填得出
--    这一列(与 quota_usage.id 同类问题)。
--
--    outbox_message.id 尤其关键: 03§8.2 要求事件与订单行同事务写入,这张表写不进
--    去,支付回调的状态迁移就没法带上事件。
--
-- 2. order_auto_renew — 自动续费开关
--    该开关由续费调度器读取并铸出 RENEW 订单,是真实的客户状态,但 V1 没有这张表,
--    内存实现把它放在 map 里,重启即丢 —— 客户设了自动续费,重启后资源到期不续,
--    这是直接的服务事故。补表,主键 (account_id, resource_id)。
--
-- 号段行('order_main')由 trade_db V2(svc-catalog,manifest 首目录)统一 seed,
-- 见该文件的说明。
-- =============================================================================

ALTER TABLE `order_state_log`
  MODIFY COLUMN `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;

ALTER TABLE `outbox_message`
  MODIFY COLUMN `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;

ALTER TABLE `idempotent_record`
  MODIFY COLUMN `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;

CREATE TABLE `order_auto_renew` (
  `account_id`   BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `resource_id`  VARCHAR(64) NOT NULL COMMENT '续费对象资源',
  `product_code` VARCHAR(32) NOT NULL COMMENT '随行冗余;控制台免二次查询即可标注',
  `enabled`      TINYINT NOT NULL DEFAULT 1 COMMENT '1开启 0关闭;关闭即删除语义由状态表达',
  `updated_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`account_id`, `resource_id`),
  KEY `idx_enabled` (`enabled`, `updated_at`) COMMENT '续费调度器扫描'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='自动续费开关;续费调度器的输入,重启不得丢失';
