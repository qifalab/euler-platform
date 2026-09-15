-- =============================================================================
-- metering_db V2 — 代理主键补 AUTO_INCREMENT
--
-- 五张表的 BIGINT 主键都是纯代理键,业务身份分别是
--   metering_record        → uk_res_item_hour / uk_agg_id(派生幂等键)
--   metering_backfill_task → uk_target(同一小时只补一次)
--   bill_detail            → uk_charge_id(chg-{agg_id},结算幂等键)
--   bill_main              → uk_acc_period(账户+月账期)
--   recon_report           → recon_date+account(对账日)
-- 领域结构里都没有这些 id 字段 —— 没有 AUTO_INCREMENT 就没有任何写入方填得出
-- 这一列。与 quota_usage/resource_state_log/api_action 是同一类缺陷。
-- =============================================================================

ALTER TABLE `metering_record`
  MODIFY COLUMN `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;

ALTER TABLE `metering_backfill_task`
  MODIFY COLUMN `task_id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;

ALTER TABLE `bill_detail`
  MODIFY COLUMN `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;

ALTER TABLE `bill_main`
  MODIFY COLUMN `bill_id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;

ALTER TABLE `recon_report`
  MODIFY COLUMN `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;
