-- =============================================================================
-- svc-billing DDL — trade_db (发票落库补齐, 二期 M-4.3)
--
-- V3 建了 invoice / invoice_item,但发票簿一直只在进程内存里:重启即丢,两个副本
-- 各有一本互不相识的发票簿 —— 同一份已消费额度可以在两个副本上各开一次票. 本文件
-- 补齐让发票落库所缺的两处:
--
--   1. invoice_item.id 补 AUTO_INCREMENT: 纯代理键(业务身份是 uk_invoice_seq),
--      领域结构里没有行 id,没有 AUTO_INCREMENT 就没有写入方填得出这一列.
--   2. invoice_lock —— 账户级开票串行锁. "开票金额不超过已消费金额减去已开票金额"
--      是跨行约束(账户的全部有效发票),行级乐观锁守不住两个并发草稿各占一半额度;
--      开票/改草稿/开具/红冲都在事务里先 SELECT ... FOR UPDATE 这一行,同一账户的
--      额度校验与写入在所有副本间串行. 行在首次开票时 INSERT IGNORE 创建,不承载数据.
--
-- Sharding: account_id 单键 (04§6.3). 可重入: MODIFY 与 CREATE TABLE IF NOT EXISTS
-- 重复执行无副作用.
-- =============================================================================

ALTER TABLE `invoice_item`
  MODIFY COLUMN `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT;

CREATE TABLE IF NOT EXISTS `invoice_lock` (
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '分片键;每账户一行',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='账户级开票锁;跨副本串行化同一账户的开票额度校验与写入';
