-- =============================================================================
-- trade_db V3 (svc-order) — 订单用券流水 order_coupon_use
--
-- t_coupon (svc-catalog V1/V3) 是券本身;它没有"被哪张订单占用/消耗了多少"的
-- 落点,试用券因此领了也用不掉(铸在进程内存里,下单也不认).本表是券在订单链路上
-- 的唯一事实来源,每 (券, 订单) 一行:
--
--   下单  INSERT state=0 LOCKED   —— 与 order_main 同事务;券被该订单预占
--   支付  LOCKED   → CONSUMED(1)  —— 与 PENDING_PAYMENT→PAID 同事务;
--                                     t_coupon.remain_value 按 amount 扣减(乐观锁 version)
--   取消  LOCKED   → RELEASED(2)  —— 与 →CANCELLED 同事务;券原样归还
--   退款  CONSUMED → RESTORED(3)  —— 与 REFUNDING→REFUNDED 同事务;remain_value 加回
--
-- 每一步只认前一状态 (UPDATE ... WHERE state=?),重放即空操作 —— (coupon, order)
-- 粒度幂等. uk_locked_coupon 是"一张券同一时刻只被一张未支付订单预占"的表级兜底:
-- locked_coupon_id 仅在 LOCKED 行非空,唯一索引忽略 NULL.
--
-- 金额 DECIMAL(10,2) 与 t_coupon.face_value/remain_value 同宽;订单面按分结算.
-- Sharding: account_id 单键 (04§6.3). 可重入: CREATE TABLE IF NOT EXISTS.
-- =============================================================================

CREATE TABLE IF NOT EXISTS `order_coupon_use` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '代理主键;业务身份是 uk_coupon_order',
  `coupon_id`        VARCHAR(32) NOT NULL COMMENT 't_coupon.coupon_id',
  `order_id`         BIGINT UNSIGNED NOT NULL COMMENT 'order_main.order_id',
  `account_id`       BIGINT UNSIGNED NOT NULL COMMENT '分片键;券与订单同属该账号',
  `amount`           DECIMAL(10,2) NOT NULL COMMENT '本单对该券的抵扣额(元)',
  `state`            TINYINT NOT NULL DEFAULT 0 COMMENT '0LOCKED已预占 1CONSUMED已消耗 2RELEASED已释放 3RESTORED已退回',
  `locked_coupon_id` VARCHAR(32) GENERATED ALWAYS AS (CASE WHEN `state` = 0 THEN `coupon_id` END) STORED
                     COMMENT '仅 LOCKED 行非空;唯一索引保证一券只被一单预占',
  `created_at`       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_coupon_order` (`coupon_id`,`order_id`) COMMENT '(券,订单) 幂等',
  UNIQUE KEY `uk_locked_coupon` (`locked_coupon_id`) COMMENT '一券同时只被一张未支付订单预占',
  KEY `idx_account_order` (`account_id`,`order_id`) COMMENT '订单详情/状态迁移按单取券',
  CONSTRAINT `ck_order_coupon_use_amount` CHECK (`amount` > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='订单用券流水;预占/消耗/释放/退回与订单状态迁移同事务';
