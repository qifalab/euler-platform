-- =============================================================================
-- trade_db V4 — t_product 补位置模型列,并修正种子数据的 placement 漂移
--
-- 两处"目录进数据库后才会爆"的缺陷:
--
-- 1. 代码域模型有 CrossAZ(M-6: ZONAL 产品的跨可用区副本/HA 标记,询价与履约都要),
--    t_product 却没有这一列 —— 落库即丢失。
-- 2. region_scope 的种子把**所有产品写成了 REGIONAL**,而代码的既定位置模型是
--    euecs/eurds/euredis/eukafka = ZONAL(创建时锁定一个 AZ,M-6 的 placement 门
--    依赖它)。目录一旦以库为事实源,ZONAL 门就会静默失效 —— 错 placement 的
--    资源不再在询价(最便宜的检查点)被拦下,而是到履约期才炸。
--    (V1 注释的词表 REGIONAL/GLOBAL 是 M-6 之前的旧词表,ZONAL 是 M-6 引入的。)
--
-- UPDATE 按代码目录的权威值逐条修正;可重复执行。
-- =============================================================================

ALTER TABLE `t_product`
  MODIFY COLUMN `region_scope` VARCHAR(16) NOT NULL DEFAULT 'REGIONAL'
    COMMENT 'REGIONAL 跨可用区 / ZONAL 创建时锁定单AZ(M-6)/ GLOBAL 全局资源(如 IAM)',
  ADD COLUMN `cross_az` TINYINT NOT NULL DEFAULT 0
    COMMENT 'M-6: ZONAL 产品的跨可用区副本(HA)标记;REGIONAL 下仅 eulb(跨AZ流量入口)为 1'
    AFTER `region_scope`;

UPDATE `t_product` SET `region_scope` = 'REGIONAL', `cross_az` = 0
  WHERE `product_code` IN ('euvpc','euoss','eumon','euas','eubackup','eulog');

UPDATE `t_product` SET `region_scope` = 'REGIONAL', `cross_az` = 1
  WHERE `product_code` = 'eulb';

UPDATE `t_product` SET `region_scope` = 'ZONAL', `cross_az` = 1
  WHERE `product_code` IN ('euecs','eurds','euredis','eukafka');

UPDATE `t_product` SET `region_scope` = 'ZONAL', `cross_az` = 0
  WHERE `product_code` IN ('eubs','eueip','eueci');
