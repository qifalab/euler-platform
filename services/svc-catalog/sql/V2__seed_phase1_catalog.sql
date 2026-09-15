-- =============================================================================
-- svc-catalog seed data — phase-1 product catalogue + phase-2 EUECI (M-7.1)
--
-- Phase-1 products (decision R-01 / adjudication C1+S1, 7):
--   EUVPC 专有网络 / EUECS 云服务器 / EUBS 块存储 / EUOSS 对象存储
--   EURDS 托管MySQL / EUMON 云监控 / EUEIP 弹性公网IP
--
-- Phase-2 (M-7.1, 09-roadmap §4.3 M-7, decision D-03):
--   EUECI 弹性容器实例 — per-second POSTPAID container instance fulfilled by
--   DriverK8s (not the VM driver). The "弹性" partner to EUECS's VM 旗舰.
--
-- Prices are illustrative placeholders for the pricing pipeline; real figures
-- come from the 定价评审 gate (09§6.2 商业门禁).
-- =============================================================================

-- --- Products (三件套 leg 1: resource type) ---------------------------------

INSERT INTO `t_product`
  (`product_code`,`product_name`,`category`,`description`,`resource_type`,`region_scope`,`status`,`owner_team`) VALUES
  ('euvpc','辰云专有网络','network',   '租户逻辑隔离网络,一切资源的网络边界',        'vpc',      'REGIONAL',2,'network-line'),
  ('euecs','辰云服务器',  'compute',   '云上虚拟服务器,一切资源的基础算力载体',      'instance', 'REGIONAL',2,'compute-line'),
  ('eubs', '辰云块存储',  'storage',   '挂载云服务器的高性能云盘',                    'disk',     'REGIONAL',2,'storage-line'),
  ('euoss','辰云对象存储','storage',   'RESTful 海量非结构化存储,S3 兼容生态锚点',   'bucket',   'REGIONAL',2,'storage-line'),
  ('eurds','辰云数据库MySQL版','database','托管 MySQL 关系型数据库,企业上云标配',   'dbinstance','REGIONAL',2,'data-line'),
  ('eumon','辰云监控',    'monitor',   '资源与自定义指标监控告警',                    'monitor',  'REGIONAL',2,'platform-line'),
  ('eueip','弹性公网IP',  'network',   '可独立购买与动态绑定的公网地址',              'eip',      'REGIONAL',2,'network-line'),
  -- EUECI 弹性容器实例 (M-7.1): ZONAL container instance, per-second postpaid.
  -- resource_type='eci' distinguishes it from 'instance' (EUECS VM).
  ('eueci','弹性容器实例','compute',   '秒级拉起的容器实例,按秒计费,弹性计算的轻量搭档','eci',     'ZONAL',    2,'compute-line'),
  -- EULB 负载均衡 (M-7.2, P0): REGIONAL, cross-AZ entry point, postpaid by usage.
  ('eulb','辰云负载均衡','network',   '四层/七层负载均衡,跨可用区流量入口',             'slb',          'REGIONAL',2,'network-line'),
  -- EUAS 弹性伸缩 (M-7.3, P2): REGIONAL scaling group (policy layer over HPA/VPA/CA).
  ('euas','弹性伸缩',  'management', '伸缩组策略引擎,基于 ECS/ECI 自动扩缩容',        'scalinggroup', 'REGIONAL',2,'compute-line'),
  -- EUBACKUP 云备份 (M-7.4, P2): REGIONAL backup policy, cross-AZ copy is a flag.
  ('eubackup','云备份', 'storage',   '定时快照与跨可用区备份策略,保留期自动清理',      'backuppolicy', 'REGIONAL',2,'storage-line'),
  -- EUREDIS 托管 Redis (M-7.5, P1): ZONAL with cross-AZ HA replica (managed DB).
  ('euredis','辰云数据库Redis版','database','托管 Redis,主备跨可用区,平台运维经验产品化','redisinstance','ZONAL',  2,'data-line'),
  -- EUKAFKA 托管 Kafka (M-7.5, P1): ZONAL with cross-AZ HA (brokers across AZs, M-6.2a kafka-kraft productized).
  ('eukafka','辰云消息队列Kafka版','middleware','托管 Kafka,跨可用区 broker,平台运维经验产品化','kafkainstance','ZONAL',2,'data-line'),
  -- EULOG 日志服务 (M-7.5, P1): REGIONAL ingestion + storage (Vector+ClickHouse), cross-AZ storage is a replica flag.
  ('eulog','辰云日志服务','middleware','日志采集与存储,Vector+ClickHouse,多租户隔离','loginstance','REGIONAL',2,'data-line');

-- --- Metering items (三件套 leg 2) ------------------------------------------
--
-- Architecture principle 6: 一切可计量 — a product with no metering item
-- cannot be listed. Precision is SECOND where 按量 billing measures usage
-- continuously, HOUR where the meter is a periodic snapshot.

INSERT INTO `t_metering_item`
  (`item_code`,`product_code`,`metric_name`,`unit`,`precision_unit`,`collect_mode`,`collect_period`,`billing_phase`) VALUES
  -- EUECS: 计费起点为 RUNNING 时刻(03§5.3),按秒计量按小时出账
  ('euecs.cpu_core_hour', 'euecs','cpu_core_hour','second','SECOND','PUSH',60,'POSTPAID'),
  ('euecs.mem_gb_hour',   'euecs','mem_gb_hour',  'second','SECOND','PUSH',60,'POSTPAID'),
  -- EUBS: 按容量小时计费
  ('eubs.disk_gb_hour',   'eubs', 'disk_gb_hour', 'gb_hour','HOUR', 'PULL',300,'POSTPAID'),
  ('eubs.snapshot_gb_hour','eubs','snapshot_gb_hour','gb_hour','HOUR','PULL',300,'POSTPAID'),
  -- EUOSS: 存储量 + 请求数 + 外网流出流量三条计量流(04§9.4)
  ('euoss.storage_gb_hour','euoss','storage_gb_hour','gb_hour','HOUR','PULL',300,'POSTPAID'),
  ('euoss.api_10k_req',    'euoss','api_10k_req',    'count',  'HOUR','PUSH',60, 'POSTPAID'),
  ('euoss.egress_gb',      'euoss','egress_gb',      'gb_hour','HOUR','PUSH',60, 'POSTPAID'),
  -- EURDS: 按规格 + 存储
  ('eurds.instance_hour',  'eurds','instance_hour',  'second','SECOND','PUSH',60,'POSTPAID'),
  ('eurds.storage_gb_hour','eurds','storage_gb_hour','gb_hour','HOUR', 'PULL',300,'POSTPAID'),
  -- EUEIP: 按带宽或按流量二选一
  ('eueip.bandwidth_mbps_hour','eueip','bandwidth_mbps_hour','gb_hour','HOUR','PULL',300,'POSTPAID'),
  ('eueip.traffic_gb',         'eueip','traffic_gb',         'gb_hour','HOUR','PUSH',60, 'POSTPAID'),
  -- EUVPC: 网络边界本身不计费,可计费子资源(EIP/LB)承担计量;保留一条
  -- 零费率计量项使产品满足"可计量"门禁并为二期 NAT/带宽包预留位置
  ('euvpc.vpc_hour',      'euvpc','vpc_hour','second','HOUR','PULL',300,'POSTPAID'),
  -- EUMON: 基础监控免费,10s 粒度与自定义指标为付费档(01§11 云监控)
  ('eumon.custom_metric_10k','eumon','custom_metric_10k','count','HOUR','PUSH',60,'POSTPAID'),
  -- EUECI 弹性容器实例 (M-7.1): per-SECOND metering (09 §4.2). The window is
  -- sub-minute; the aggregation job folds seconds into the hourly total that
  -- svc-billing settles. Same cpu/mem dimensions as EUECS but billed by the
  -- second, not the hour — the granularity that makes bursty/ephemeral
  -- containers economical.
  ('eueci.cpu_core_second','eueci','cpu_core_second','second','SECOND','PUSH',60,'POSTPAID'),
  ('eueci.mem_gb_second',  'eueci','mem_gb_second',  'second','SECOND','PUSH',60,'POSTPAID'),
  -- EULB 负载均衡 (M-7.2): LCU capacity unit + egress traffic.
  ('eulb.lcu_hour',        'eulb','lcu_hour',        'count',  'HOUR','PUSH',60,'POSTPAID'),
  ('eulb.traffic_gb',      'eulb','traffic_gb',      'gb_hour','HOUR','PUSH',60,'POSTPAID'),
  -- EUAS 弹性伸缩 (M-7.3): scaling-group-hour management fee (managed instances bill separately).
  ('euas.scaling_group_hour','euas','scaling_group_hour','second','HOUR','PULL',300,'POSTPAID'),
  -- EUBACKUP 云备份 (M-7.4): stored backup capacity + snapshot count.
  ('eubackup.backup_storage_gb_hour','eubackup','backup_storage_gb_hour','gb_hour','HOUR','PULL',300,'POSTPAID'),
  ('eubackup.snapshot_count','eubackup','snapshot_count','count','HOUR','PUSH',60,'POSTPAID'),
  -- EUREDIS 托管 Redis (M-7.5): memory capacity + instance-hour (managed DB).
  ('euredis.redis_mem_gb_hour','euredis','redis_mem_gb_hour','second','SECOND','PUSH',60,'POSTPAID'),
  ('euredis.redis_instance_hour','euredis','redis_instance_hour','second','SECOND','PUSH',60,'POSTPAID'),
  -- EUKAFKA 托管 Kafka (M-7.5): broker-hour (priced instance dim) + partition-hour + traffic.
  ('eukafka.broker_hour','eukafka','broker_hour','second','HOUR','PUSH',60,'POSTPAID'),
  ('eukafka.partition_hour','eukafka','partition_hour','second','HOUR','PUSH',60,'POSTPAID'),
  ('eukafka.traffic_gb','eukafka','traffic_gb','gb_hour','HOUR','PUSH',60,'POSTPAID'),
  -- EULOG 日志服务 (M-7.5): storage-hour + ingestion-by-volume (05§4.1, same shape as euoss).
  ('eulog.storage_gb_hour','eulog','storage_gb_hour','gb_hour','HOUR','PULL',300,'POSTPAID'),
  ('eulog.ingestion_gb','eulog','ingestion_gb','gb_hour','HOUR','PUSH',60,'POSTPAID');

-- --- SKUs -------------------------------------------------------------------
--
-- Phase-1 charge types only: PREPAID 包年包月 + POSTPAID 按量 (decision D6).
-- RESOURCE_PACK and SPOT rows are deliberately not seeded — the enum reserves
-- them, but an unsold charge type must not be orderable.

INSERT INTO `t_sku` (`sku_code`,`product_code`,`charge_type`,`spec_json`,`status`) VALUES
  -- EUECS 通用型
  ('euecs.s2.small.prepaid',  'euecs','PREPAID', '{"cpu":1,"mem_gb":2}','1'),
  ('euecs.s2.small.postpaid', 'euecs','POSTPAID','{"cpu":1,"mem_gb":2}','1'),
  ('euecs.s2.large.prepaid',  'euecs','PREPAID', '{"cpu":2,"mem_gb":4}','1'),
  ('euecs.s2.large.postpaid', 'euecs','POSTPAID','{"cpu":2,"mem_gb":4}','1'),
  ('euecs.s2.xlarge.prepaid', 'euecs','PREPAID', '{"cpu":4,"mem_gb":8}','1'),
  ('euecs.s2.xlarge.postpaid','euecs','POSTPAID','{"cpu":4,"mem_gb":8}','1'),
  -- EUBS 云盘
  ('eubs.essd.prepaid',       'eubs', 'PREPAID', '{"disk_type":"essd","min_gb":20}','1'),
  ('eubs.essd.postpaid',      'eubs', 'POSTPAID','{"disk_type":"essd","min_gb":20}','1'),
  -- EUOSS 标准存储(按量为主)
  ('euoss.standard.postpaid', 'euoss','POSTPAID','{"storage_class":"standard"}','1'),
  -- EURDS 托管 MySQL 一主一备
  ('eurds.mysql8.small.prepaid', 'eurds','PREPAID', '{"engine":"mysql","version":"8.0","cpu":2,"mem_gb":4,"ha":true}','1'),
  ('eurds.mysql8.small.postpaid','eurds','POSTPAID','{"engine":"mysql","version":"8.0","cpu":2,"mem_gb":4,"ha":true}','1'),
  -- EUEIP 按带宽 / 按流量
  ('eueip.bandwidth.prepaid', 'eueip','PREPAID', '{"billing":"bandwidth","mbps":5}','1'),
  ('eueip.traffic.postpaid',  'eueip','POSTPAID','{"billing":"traffic"}','1'),
  -- EUVPC 免费
  ('euvpc.standard.postpaid', 'euvpc','POSTPAID','{"tier":"standard"}','1'),
  -- EUMON 基础免费档
  ('eumon.basic.postpaid',    'eumon','POSTPAID','{"granularity_s":60}','1'),
  -- EUECI 弹性容器实例 (M-7.1): POSTPAID only — a prepaid container would
  -- just be a VM (09 §3.2 D-03). cpu/mem_gb mirror EUECS specs.
  ('eueci.c2.small.postpaid', 'eueci','POSTPAID','{"cpu":1,"mem_gb":2}','1'),
  ('eueci.c2.large.postpaid', 'eueci','POSTPAID','{"cpu":2,"mem_gb":4}','1'),
  ('eueci.c2.xlarge.postpaid','eueci','POSTPAID','{"cpu":4,"mem_gb":8}','1'),
  -- EULB 负载均衡 (M-7.2): POSTPAID by usage tier (L7 by QPS, L4 by conn).
  ('eulb.l1.small.postpaid',   'eulb','POSTPAID','{"type":"l7","max_qps":100}','1'),
  ('eulb.l1.large.postpaid',  'eulb','POSTPAID','{"type":"l7","max_qps":5000}','1'),
  ('eulb.l4.conn.postpaid',   'eulb','POSTPAID','{"type":"l4","max_conn":100000}','1'),
  -- EUAS 弹性伸缩 (M-7.3): POSTPAID management fee (managed ECS/ECI bill separately).
  ('euas.standard.postpaid',  'euas','POSTPAID','{"managed_type":"ecs"}','1'),
  ('euas.eci.postpaid',       'euas','POSTPAID','{"managed_type":"eci"}','1'),
  -- EUBACKUP 云备份 (M-7.4): POSTPAID by stored capacity.
  ('eubackup.standard.postpaid','eubackup','POSTPAID','{"tier":"standard"}','1'),
  ('eubackup.crossaz.postpaid', 'eubackup','POSTPAID','{"tier":"crossaz","cross_az":true}','1'),
  -- EUREDIS 托管 Redis (M-7.5): both prepay and postpay (managed DB, like eurds).
  ('euredis.redis.small.prepaid', 'euredis','PREPAID', '{"engine":"redis","version":"7.0","mem_gb":1,"ha":true}','1'),
  ('euredis.redis.small.postpaid','euredis','POSTPAID','{"engine":"redis","version":"7.0","mem_gb":1,"ha":true}','1'),
  ('euredis.redis.large.prepaid', 'euredis','PREPAID', '{"engine":"redis","version":"7.0","mem_gb":4,"ha":true}','1'),
  ('euredis.redis.large.postpaid','euredis','POSTPAID','{"engine":"redis","version":"7.0","mem_gb":4,"ha":true}','1'),
  -- EUKAFKA 托管 Kafka (M-7.5): both prepay and postpay (managed middleware).
  ('eukafka.kafka.standard.prepaid', 'eukafka','PREPAID', '{"engine":"kafka","version":"3.7","broker_count":3,"cross_az":true}','1'),
  ('eukafka.kafka.standard.postpaid','eukafka','POSTPAID','{"engine":"kafka","version":"3.7","broker_count":3,"cross_az":true}','1'),
  ('eukafka.kafka.large.prepaid',  'eukafka','PREPAID', '{"engine":"kafka","version":"3.7","broker_count":5,"cross_az":true}','1'),
  ('eukafka.kafka.large.postpaid', 'eukafka','POSTPAID','{"engine":"kafka","version":"3.7","broker_count":5,"cross_az":true}','1'),
  -- EULOG 日志服务 (M-7.5): REGIONAL, postpaid by ingestion + storage.
  ('eulog.log.standard.postpaid','eulog','POSTPAID','{"tier":"standard","retention_days":7,"storage_gb":50}','1'),
  ('eulog.log.pro.postpaid',     'eulog','POSTPAID','{"tier":"pro","retention_days":30,"storage_gb":500,"cross_az":true}','1');

-- --- Pricing rules ----------------------------------------------------------
--
-- APPEND-ONLY: a price change inserts a new row and closes the old one's
-- effective_to. Never UPDATE list_price in place (01§12.3 rule 3).
--
-- effective_from is set in the past so the seed is immediately quotable.

INSERT INTO `t_pricing_rule`
  (`sku_code`,`region_id`,`duration_unit`,`list_price`,`currency`,`customer_level`,`effective_from`,`created_by`) VALUES
  -- EUECS 包年包月(元/月)
  ('euecs.s2.small.prepaid', '*','MONTH', 90.000000,'CNY','','2026-01-01 00:00:00','seed'),
  ('euecs.s2.large.prepaid', '*','MONTH',180.000000,'CNY','','2026-01-01 00:00:00','seed'),
  ('euecs.s2.xlarge.prepaid','*','MONTH',360.000000,'CNY','','2026-01-01 00:00:00','seed'),
  -- 企业客户等级差价示例:同 SKU 更低目录价,规则更具体故优先命中
  ('euecs.s2.large.prepaid', '*','MONTH',150.000000,'CNY','ENTERPRISE','2026-01-01 00:00:00','seed'),
  -- 地域差价示例:cn-east-1 更贵
  ('euecs.s2.large.prepaid','cn-east-1','MONTH',200.000000,'CNY','','2026-01-01 00:00:00','seed'),
  -- EUECS 按量(元/小时)
  ('euecs.s2.small.postpaid', '*','HOUR',0.125000,'CNY','','2026-01-01 00:00:00','seed'),
  ('euecs.s2.large.postpaid', '*','HOUR',0.250000,'CNY','','2026-01-01 00:00:00','seed'),
  ('euecs.s2.xlarge.postpaid','*','HOUR',0.500000,'CNY','','2026-01-01 00:00:00','seed'),
  -- EUBS(元/GB/月, 元/GB/小时)
  ('eubs.essd.prepaid', '*','MONTH',1.000000,'CNY','','2026-01-01 00:00:00','seed'),
  ('eubs.essd.postpaid','*','HOUR', 0.001400,'CNY','','2026-01-01 00:00:00','seed'),
  -- EUOSS(元/GB/小时)
  ('euoss.standard.postpaid','*','HOUR',0.000170,'CNY','','2026-01-01 00:00:00','seed'),
  -- EURDS
  ('eurds.mysql8.small.prepaid', '*','MONTH',420.000000,'CNY','','2026-01-01 00:00:00','seed'),
  ('eurds.mysql8.small.postpaid','*','HOUR',  0.700000,'CNY','','2026-01-01 00:00:00','seed'),
  -- EUEIP
  ('eueip.bandwidth.prepaid','*','MONTH',23.000000,'CNY','','2026-01-01 00:00:00','seed'),
  ('eueip.traffic.postpaid', '*','HOUR',  0.800000,'CNY','','2026-01-01 00:00:00','seed'),
  -- EUVPC / EUMON 基础档零费率:产品可计量、可下单,但不产生费用
  ('euvpc.standard.postpaid','*','HOUR',0.000000,'CNY','','2026-01-01 00:00:00','seed'),
  ('eumon.basic.postpaid',   '*','HOUR',0.000000,'CNY','','2026-01-01 00:00:00','seed'),
  -- EUECI 弹性容器实例 (M-7.1): per-SECOND billing — duration_unit SECOND
  -- (the unit M-4.2 reserved for this, 09 §4.2). 0.00007 yuan/sec × 3600 =
  -- 0.252/hr, matching the euecs.s2.large hourly band (compute-parity).
  ('eueci.c2.small.postpaid', '*','SECOND',0.000035,'CNY','','2026-01-01 00:00:00','seed'),
  ('eueci.c2.large.postpaid', '*','SECOND',0.000070,'CNY','','2026-01-01 00:00:00','seed'),
  ('eueci.c2.xlarge.postpaid','*','SECOND',0.000140,'CNY','','2026-01-01 00:00:00','seed'),
  -- EULB 负载均衡 (M-7.2): per-hour postpaid by usage tier.
  ('eulb.l1.small.postpaid', '*','HOUR',0.060000,'CNY','','2026-01-01 00:00:00','seed'),
  ('eulb.l1.large.postpaid','*','HOUR',0.300000,'CNY','','2026-01-01 00:00:00','seed'),
  ('eulb.l4.conn.postpaid', '*','HOUR',0.120000,'CNY','','2026-01-01 00:00:00','seed'),
  -- EUAS 弹性伸缩 (M-7.3): per-hour management fee.
  ('euas.standard.postpaid','*','HOUR',0.020000,'CNY','','2026-01-01 00:00:00','seed'),
  ('euas.eci.postpaid',     '*','HOUR',0.020000,'CNY','','2026-01-01 00:00:00','seed'),
  -- EUBACKUP 云备份 (M-7.4): per-hour by stored capacity.
  ('eubackup.standard.postpaid','*','HOUR',0.000700,'CNY','','2026-01-01 00:00:00','seed'),
  ('eubackup.crossaz.postpaid', '*','HOUR',0.001400,'CNY','','2026-01-01 00:00:00','seed'),
  -- EUREDIS 托管 Redis (M-7.5): prepay 元/月 + postpay 元/小时.
  ('euredis.redis.small.prepaid', '*','MONTH',78.000000,'CNY','','2026-01-01 00:00:00','seed'),
  ('euredis.redis.small.postpaid','*','HOUR', 0.160000,'CNY','','2026-01-01 00:00:00','seed'),
  ('euredis.redis.large.prepaid', '*','MONTH',300.000000,'CNY','','2026-01-01 00:00:00','seed'),
  ('euredis.redis.large.postpaid','*','HOUR', 0.620000,'CNY','','2026-01-01 00:00:00','seed'),
  -- EUKAFKA 托管 Kafka (M-7.5): prepay 元/月 + postpay 元/小时 (managed middleware).
  ('eukafka.kafka.standard.prepaid', '*','MONTH',450.000000,'CNY','','2026-01-01 00:00:00','seed'),
  ('eukafka.kafka.standard.postpaid','*','HOUR', 0.850000,'CNY','','2026-01-01 00:00:00','seed'),
  ('eukafka.kafka.large.prepaid',  '*','MONTH',1200.000000,'CNY','','2026-01-01 00:00:00','seed'),
  ('eukafka.kafka.large.postpaid', '*','HOUR', 2.400000,'CNY','','2026-01-01 00:00:00','seed'),
  -- EULOG 日志服务 (M-7.5): postpaid by storage-hour (ingestion_gb metered by USAGE).
  ('eulog.log.standard.postpaid','*','HOUR',0.005000,'CNY','','2026-01-01 00:00:00','seed'),
  ('eulog.log.pro.postpaid',     '*','HOUR',0.020000,'CNY','','2026-01-01 00:00:00','seed');

-- --- Promotions -------------------------------------------------------------
-- 新客首购 3 折 (01§4.6 转化漏斗). Promotions never stack; 询价 picks the best
-- single applicable one.

INSERT INTO `t_promo_policy`
  (`promo_id`,`promo_name`,`promo_type`,`scope_type`,`scope_ref`,`rate_bp`,`user_tag`,`budget_total`,`start_at`,`end_at`,`status`) VALUES
  ('promo-newuser-2026','新客首购3折','DISCOUNT_RATE','ORDER','',3000,'new',1000000.00,
   '2026-01-01 00:00:00','2027-01-01 00:00:00',1),
  ('promo-ecs-annual','云服务器包年85折','DISCOUNT_RATE','PRODUCT','euecs',8500,'',500000.00,
   '2026-01-01 00:00:00','2027-01-01 00:00:00',1);
