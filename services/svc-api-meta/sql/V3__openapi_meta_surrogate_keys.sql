-- =============================================================================
-- openapi_meta V3 — 代理主键补 AUTO_INCREMENT
--
-- api_action.action_id 与 api_doc.doc_id 都是纯代理键:业务身份分别是
-- uk_product_action_version(product, action, version)与 uk_product_version,
-- 领域结构里没有这两个 id 字段(域的身份是 "{product}.{Action}" 字符串)。
-- 没有 AUTO_INCREMENT 就没有任何写入方填得出这一列 —— 与 quota_usage.id、
-- resource_state_log.id 是同一类"从未被真实执行过才藏得住"的缺陷。
-- =============================================================================

ALTER TABLE `api_action`
  MODIFY COLUMN `action_id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;

ALTER TABLE `api_doc`
  MODIFY COLUMN `doc_id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;
