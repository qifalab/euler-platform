-- =============================================================================
-- support_db V4 — 代理主键补 AUTO_INCREMENT
--
-- notification_delivery.id 是纯代理键,业务身份是 idx_notification(notification_id)
-- + 逐渠道一行;领域结构里没有这个 id 字段。没有 AUTO_INCREMENT 就没有任何写入方
-- 填得出这一列 —— 与 quota_usage/resource_state_log/api_action 同类缺陷。
-- =============================================================================

ALTER TABLE `notification_delivery`
  MODIFY COLUMN `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;
