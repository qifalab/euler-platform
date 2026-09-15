-- =============================================================================
-- resource_db V3 — 代理主键补 AUTO_INCREMENT
--
-- resource_state_log.id 与 provision_task.task_id 都是纯代理键:业务身份分别是
-- idx_resource_time(resource_id, occurred_at)与 uk_idem(idempot_key, op_type),
-- 领域结构里没有这两个 id 字段 —— 没有 AUTO_INCREMENT 就没有任何写入方填得出
-- 这一列(与 quota_usage.id 同类问题)。
--
-- resource_state_log 尤其关键:03§4.3.3 要求"为什么资源在这个状态"在数月后仍可
-- 回答,写不进去等于台账没有历史。flow_instance/step_instance 的号段见本目录 V2。
-- =============================================================================

ALTER TABLE `resource_state_log`
  MODIFY COLUMN `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;

ALTER TABLE `provision_task`
  MODIFY COLUMN `task_id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;
