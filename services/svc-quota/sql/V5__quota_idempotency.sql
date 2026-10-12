-- =============================================================================
-- support_db V5 — 配额幂等键 quota_idempotency
--
-- occupy 的客户端幂等键此前只在进程内存 map + 进程内锁里: 两个副本各自 miss、
-- 各自预占一次;重启后所有键被遗忘. 现在每个带键的操作(预占 OCCUPY / 归还已
-- 用量 FREE)在改动 quota_usage(及插入 quota_token)的同一事务里插入本表一行,
-- uk_acc_op_key 裁决并发的首次请求: 变更与键要么同时存在,要么都不存在.
--
-- 一个键永远只对应一次操作: 重放返回最初的结果(预占即原 reservation,即使令牌
-- 已被确认/释放/超时回收),绝不在令牌消失后重新预占 —— 那会把已确认的预占
-- 计两次. 预占结束后要新预占的调用方使用新键.
--
-- idem_key 用 utf8mb4_0900_bin (区分大小写、NO PAD): 客户端键逐字节比较,
-- 与内存实现一致. Idempotent: CREATE TABLE IF NOT EXISTS.
-- =============================================================================

CREATE TABLE IF NOT EXISTS `quota_idempotency` (
  `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '代理主键;业务身份是 uk_acc_op_key',
  `account_id` BIGINT UNSIGNED NOT NULL COMMENT '分片键',
  `op`         VARCHAR(16) NOT NULL COMMENT 'OCCUPY | FREE;键按操作分域',
  `idem_key`   VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL COMMENT '客户端幂等键',
  `quota_code` VARCHAR(64) NOT NULL,
  `region`     VARCHAR(32) NOT NULL COMMENT 'GLOBAL 配额为 *',
  `amount`     INT NOT NULL,
  `token_id`   VARCHAR(64) NOT NULL DEFAULT '' COMMENT 'OCCUPY: 该键创建的 reservation',
  `biz_key`    VARCHAR(64) NOT NULL DEFAULT '',
  `expires_at` DATETIME DEFAULT NULL COMMENT 'OCCUPY: 原 reservation 的到期时间',
  `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_acc_op_key` (`account_id`,`op`,`idem_key`) COMMENT '幂等终裁;同键只生效一次',
  KEY `idx_token` (`token_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='配额幂等键;与用量变更同事务写入,跨副本/重启有效';
