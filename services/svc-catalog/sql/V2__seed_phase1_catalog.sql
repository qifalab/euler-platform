-- =============================================================================
-- svc-catalog seed data — phase-1 product catalogue
--
-- The 7 sellable products of decision R-01 / adjudication C1+S1:
--   SCVPC 专有网络 / SCECS 云服务器 / SCBS 块存储 / SCOSS 对象存储
--   SCRDS 托管MySQL / SCMON 云监控 / SCEIP 弹性公网IP
--
-- SCECI 弹性容器实例 is deliberately absent — deferred to phase 2 (C1+S1
-- rejected 09 R-01's original "ECI replaces VM" plan because VPC is an ECS
-- prerequisite, ECS needs block storage for its system disk, and VM is the
-- product users actually expect).
--
-- Prices are illustrative placeholders for the pricing pipeline; real figures
-- come from the 定价评审 gate (09§6.2 商业门禁).
-- =============================================================================

-- --- Products (三件套 leg 1: resource type) ---------------------------------

INSERT INTO `t_product`
  (`product_code`,`product_name`,`category`,`description`,`resource_type`,`region_scope`,`status`,`owner_team`) VALUES
  ('scvpc','辰云专有网络','network',   '租户逻辑隔离网络,一切资源的网络边界',        'vpc',      'REGIONAL',2,'network-line'),
  ('scecs','辰云服务器',  'compute',   '云上虚拟服务器,一切资源的基础算力载体',      'instance', 'REGIONAL',2,'compute-line'),
  ('scbs', '辰云块存储',  'storage',   '挂载云服务器的高性能云盘',                    'disk',     'REGIONAL',2,'storage-line'),
  ('scoss','辰云对象存储','storage',   'RESTful 海量非结构化存储,S3 兼容生态锚点',   'bucket',   'REGIONAL',2,'storage-line'),
  ('scrds','辰云数据库MySQL版','database','托管 MySQL 关系型数据库,企业上云标配',   'dbinstance','REGIONAL',2,'data-line'),
  ('scmon','辰云监控',    'monitor',   '资源与自定义指标监控告警',                    'monitor',  'REGIONAL',2,'platform-line'),
  ('sceip','弹性公网IP',  'network',   '可独立购买与动态绑定的公网地址',              'eip',      'REGIONAL',2,'network-line');

-- --- Metering items (三件套 leg 2) ------------------------------------------
--
-- Architecture principle 6: 一切可计量 — a product with no metering item
-- cannot be listed. Precision is SECOND where 按量 billing measures usage
-- continuously, HOUR where the meter is a periodic snapshot.

INSERT INTO `t_metering_item`
  (`item_code`,`product_code`,`metric_name`,`unit`,`precision_unit`,`collect_mode`,`collect_period`,`billing_phase`) VALUES
  -- SCECS: 计费起点为 RUNNING 时刻(03§5.3),按秒计量按小时出账
  ('scecs.cpu_core_hour', 'scecs','cpu_core_hour','second','SECOND','PUSH',60,'POSTPAID'),
  ('scecs.mem_gb_hour',   'scecs','mem_gb_hour',  'second','SECOND','PUSH',60,'POSTPAID'),
  -- SCBS: 按容量小时计费
  ('scbs.disk_gb_hour',   'scbs', 'disk_gb_hour', 'gb_hour','HOUR', 'PULL',300,'POSTPAID'),
  ('scbs.snapshot_gb_hour','scbs','snapshot_gb_hour','gb_hour','HOUR','PULL',300,'POSTPAID'),
  -- SCOSS: 存储量 + 请求数 + 外网流出流量三条计量流(04§9.4)
  ('scoss.storage_gb_hour','scoss','storage_gb_hour','gb_hour','HOUR','PULL',300,'POSTPAID'),
  ('scoss.api_10k_req',    'scoss','api_10k_req',    'count',  'HOUR','PUSH',60, 'POSTPAID'),
  ('scoss.egress_gb',      'scoss','egress_gb',      'gb_hour','HOUR','PUSH',60, 'POSTPAID'),
  -- SCRDS: 按规格 + 存储
  ('scrds.instance_hour',  'scrds','instance_hour',  'second','SECOND','PUSH',60,'POSTPAID'),
  ('scrds.storage_gb_hour','scrds','storage_gb_hour','gb_hour','HOUR', 'PULL',300,'POSTPAID'),
  -- SCEIP: 按带宽或按流量二选一
  ('sceip.bandwidth_mbps_hour','sceip','bandwidth_mbps_hour','gb_hour','HOUR','PULL',300,'POSTPAID'),
  ('sceip.traffic_gb',         'sceip','traffic_gb',         'gb_hour','HOUR','PUSH',60, 'POSTPAID'),
  -- SCVPC: 网络边界本身不计费,可计费子资源(EIP/LB)承担计量;保留一条
  -- 零费率计量项使产品满足"可计量"门禁并为二期 NAT/带宽包预留位置
  ('scvpc.vpc_hour',      'scvpc','vpc_hour','second','HOUR','PULL',300,'POSTPAID'),
  -- SCMON: 基础监控免费,10s 粒度与自定义指标为付费档(01§11 云监控)
  ('scmon.custom_metric_10k','scmon','custom_metric_10k','count','HOUR','PUSH',60,'POSTPAID');

-- --- SKUs -------------------------------------------------------------------
--
-- Phase-1 charge types only: PREPAID 包年包月 + POSTPAID 按量 (decision D6).
-- RESOURCE_PACK and SPOT rows are deliberately not seeded — the enum reserves
-- them, but an unsold charge type must not be orderable.

INSERT INTO `t_sku` (`sku_code`,`product_code`,`charge_type`,`spec_json`,`status`) VALUES
  -- SCECS 通用型
  ('scecs.s2.small.prepaid',  'scecs','PREPAID', '{"cpu":1,"mem_gb":2}','1'),
  ('scecs.s2.small.postpaid', 'scecs','POSTPAID','{"cpu":1,"mem_gb":2}','1'),
  ('scecs.s2.large.prepaid',  'scecs','PREPAID', '{"cpu":2,"mem_gb":4}','1'),
  ('scecs.s2.large.postpaid', 'scecs','POSTPAID','{"cpu":2,"mem_gb":4}','1'),
  ('scecs.s2.xlarge.prepaid', 'scecs','PREPAID', '{"cpu":4,"mem_gb":8}','1'),
  ('scecs.s2.xlarge.postpaid','scecs','POSTPAID','{"cpu":4,"mem_gb":8}','1'),
  -- SCBS 云盘
  ('scbs.essd.prepaid',       'scbs', 'PREPAID', '{"disk_type":"essd","min_gb":20}','1'),
  ('scbs.essd.postpaid',      'scbs', 'POSTPAID','{"disk_type":"essd","min_gb":20}','1'),
  -- SCOSS 标准存储(按量为主)
  ('scoss.standard.postpaid', 'scoss','POSTPAID','{"storage_class":"standard"}','1'),
  -- SCRDS 托管 MySQL 一主一备
  ('scrds.mysql8.small.prepaid', 'scrds','PREPAID', '{"engine":"mysql","version":"8.0","cpu":2,"mem_gb":4,"ha":true}','1'),
  ('scrds.mysql8.small.postpaid','scrds','POSTPAID','{"engine":"mysql","version":"8.0","cpu":2,"mem_gb":4,"ha":true}','1'),
  -- SCEIP 按带宽 / 按流量
  ('sceip.bandwidth.prepaid', 'sceip','PREPAID', '{"billing":"bandwidth","mbps":5}','1'),
  ('sceip.traffic.postpaid',  'sceip','POSTPAID','{"billing":"traffic"}','1'),
  -- SCVPC 免费
  ('scvpc.standard.postpaid', 'scvpc','POSTPAID','{"tier":"standard"}','1'),
  -- SCMON 基础免费档
  ('scmon.basic.postpaid',    'scmon','POSTPAID','{"granularity_s":60}','1');

-- --- Pricing rules ----------------------------------------------------------
--
-- APPEND-ONLY: a price change inserts a new row and closes the old one's
-- effective_to. Never UPDATE list_price in place (01§12.3 rule 3).
--
-- effective_from is set in the past so the seed is immediately quotable.

INSERT INTO `t_pricing_rule`
  (`sku_code`,`region_id`,`duration_unit`,`list_price`,`currency`,`customer_level`,`effective_from`,`created_by`) VALUES
  -- SCECS 包年包月(元/月)
  ('scecs.s2.small.prepaid', '*','MONTH', 90.000000,'CNY','','2026-01-01 00:00:00','seed'),
  ('scecs.s2.large.prepaid', '*','MONTH',180.000000,'CNY','','2026-01-01 00:00:00','seed'),
  ('scecs.s2.xlarge.prepaid','*','MONTH',360.000000,'CNY','','2026-01-01 00:00:00','seed'),
  -- 企业客户等级差价示例:同 SKU 更低目录价,规则更具体故优先命中
  ('scecs.s2.large.prepaid', '*','MONTH',150.000000,'CNY','ENTERPRISE','2026-01-01 00:00:00','seed'),
  -- 地域差价示例:cn-east-1 更贵
  ('scecs.s2.large.prepaid','cn-east-1','MONTH',200.000000,'CNY','','2026-01-01 00:00:00','seed'),
  -- SCECS 按量(元/小时)
  ('scecs.s2.small.postpaid', '*','HOUR',0.125000,'CNY','','2026-01-01 00:00:00','seed'),
  ('scecs.s2.large.postpaid', '*','HOUR',0.250000,'CNY','','2026-01-01 00:00:00','seed'),
  ('scecs.s2.xlarge.postpaid','*','HOUR',0.500000,'CNY','','2026-01-01 00:00:00','seed'),
  -- SCBS(元/GB/月, 元/GB/小时)
  ('scbs.essd.prepaid', '*','MONTH',1.000000,'CNY','','2026-01-01 00:00:00','seed'),
  ('scbs.essd.postpaid','*','HOUR', 0.001400,'CNY','','2026-01-01 00:00:00','seed'),
  -- SCOSS(元/GB/小时)
  ('scoss.standard.postpaid','*','HOUR',0.000170,'CNY','','2026-01-01 00:00:00','seed'),
  -- SCRDS
  ('scrds.mysql8.small.prepaid', '*','MONTH',420.000000,'CNY','','2026-01-01 00:00:00','seed'),
  ('scrds.mysql8.small.postpaid','*','HOUR',  0.700000,'CNY','','2026-01-01 00:00:00','seed'),
  -- SCEIP
  ('sceip.bandwidth.prepaid','*','MONTH',23.000000,'CNY','','2026-01-01 00:00:00','seed'),
  ('sceip.traffic.postpaid', '*','HOUR',  0.800000,'CNY','','2026-01-01 00:00:00','seed'),
  -- SCVPC / SCMON 基础档零费率:产品可计量、可下单,但不产生费用
  ('scvpc.standard.postpaid','*','HOUR',0.000000,'CNY','','2026-01-01 00:00:00','seed'),
  ('scmon.basic.postpaid',   '*','HOUR',0.000000,'CNY','','2026-01-01 00:00:00','seed');

-- --- Promotions -------------------------------------------------------------
-- 新客首购 3 折 (01§4.6 转化漏斗). Promotions never stack; 询价 picks the best
-- single applicable one.

INSERT INTO `t_promo_policy`
  (`promo_id`,`promo_name`,`promo_type`,`scope_type`,`scope_ref`,`rate_bp`,`user_tag`,`budget_total`,`start_at`,`end_at`,`status`) VALUES
  ('promo-newuser-2026','新客首购3折','DISCOUNT_RATE','ORDER','',3000,'new',1000000.00,
   '2026-01-01 00:00:00','2027-01-01 00:00:00',1),
  ('promo-ecs-annual','云服务器包年85折','DISCOUNT_RATE','PRODUCT','scecs',8500,'',500000.00,
   '2026-01-01 00:00:00','2027-01-01 00:00:00',1);
