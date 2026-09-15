-- =============================================================================
-- trade_db V2 — marketplace 号段行
--
-- V1 说得很明白:listing_id 是"全局自增,服务内序列",settlement_id 同理。但"服务内
-- 序列"在内存模式下就是进程内的一个计数器:接上真实库后重启即从 1 重新发号,第一条
-- 新商品或新结算就会撞主键(marketplace_settlement 还会因此丢掉一笔分账记录)。
--
-- 所以两个 id 都改由 trade_db 的号段分配器发放(trade_db.id_sequence 由 svc-payment V2
-- 创建,本目录只 INSERT 自己需要的序列行,不重复建表)。
--
-- 金额列不需要改动:DECIMAL(18,6) 与 pricing.Amount 的 micro-unit 标度一致,partner_micro
-- + platform_micro ≡ gross_micro 的不变量由 pkg-go/settlement.Settle 精确派生保证。
-- =============================================================================

INSERT IGNORE INTO `id_sequence` (`name`, `next_value`, `segment_size`) VALUES
  ('marketplace_listing', 1, 1000),
  ('marketplace_settlement', 1, 1000);
