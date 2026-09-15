-- =============================================================================
-- resource_db V2 — 号段分配器 (04§6.6)
--
-- resource_db 是 flow_instance / step_instance(引擎实例)与 resource_instance(资源台账)
-- 的共同schema,这些表的主键都是号段发号的 BIGINT:
--   flow_instance.flow_instance_id、step_instance.step_id  → svc-workflow 取用
--   resource_instance 等                                   → svc-orchestrator 取用
--
-- 与 trade_db / support_db / account_db 同样的做法:每个逻辑库各自持有分配器状态,
-- 不跨库取号。表由拥有本 schema DDL 的目录(services/svc-orchestrator/sql)创建一次,
-- 其他消费方只按名字取号,不再重复建表(与 outbox_message 的单一权威定义同理)。
--
-- 为什么必须落库:两个服务的 id 原本由进程内计数器发放,重启即从 1 重来 —— 第一条
-- 新流程/新资源就会撞主键;流程实例撞主键意味着一个订单的履约记录丢失。
-- =============================================================================

CREATE TABLE IF NOT EXISTS `id_sequence` (
  `name`         VARCHAR(64) NOT NULL COMMENT '序列名;约定为消费它的表',
  `next_value`   BIGINT UNSIGNED NOT NULL COMMENT '下一个可发放的号段起点',
  `segment_size` BIGINT UNSIGNED NOT NULL DEFAULT 1000 COMMENT '号段长度;未用完的部分作废',
  `updated_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  COMMENT='号段分配器状态(04§6.6);业务主键不依赖 AUTO_INCREMENT';

INSERT IGNORE INTO `id_sequence` (`name`, `next_value`, `segment_size`) VALUES
  ('flow_instance', 1, 1000),
  ('step_instance', 1, 1000);
