// Package main: svc-catalog — product/SKU registry + 询价 link
// (03-backend-services.md §4.2.1, 01-product-catalog.md §7).
//
// Owns the catalogue tables (product, sku, pricing_rule, promo_policy) and
// exposes the pricing link that the order service, console-bff and every
// checkout flow call before creating an order. It does NOT price on its own
// authority: every quote is computed by the stateless pricing engine in
// pkg-go/pricing against rules this service supplies, so the calculation order
// (目录价 → 促销折扣 → 代金券, 01§12.3) is defined once and cannot drift.
//
// Routes (gateway-authorized, X-Sc-Account-Id injected):
//
//	POST  /api/v1/catalog/quote    — 询价: runs the real pricing engine, returns pricing.Result
//	GET   /api/v1/catalog/products — list seeded products (scecs/scoss/scvpc/scrds/scmon/sceip/scbs)
//	GET   /api/v1/catalog/skus     — list SKUs, filter by ?productCode=
//	GET   /api/v1/catalog/placement — placement contract (scope/crossAz/zoneRequired), M-6
//	GET   /healthz, /readyz
//
// stdlib-HTTP service. Envelope {RequestId,Code,Data} (03§9.3). In-memory store
// (MySQL t_product/t_sku/t_pricing_rule/t_promo_policy in production). The seed
// mirrors sql/V2__seed_phase1_catalog.sql — when the SQL seed changes, the
// catalogue() below changes with it (the pricing-demo enforces the same).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/starcloud/sc-platform/pricing"
	"github.com/starcloud/sc-platform/topology"
)

const accountIDHeader = "X-Sc-Account-Id"

// product mirrors t_product (三件套 leg 1: resource type). RegionScope is the
// P2 placement scope (00§4.4, 09§4.3 M-6): REGIONAL resources spread across AZs
// (a bucket, a VPC), ZONAL resources pin to one AZ at create time (a VM). The
// field existed in phase 1 as a reserved string that was always "REGIONAL";
// M-6 makes it a real constraint the quote path enforces.
type product struct {
	ProductCode  string `json:"productCode"`
	ProductName  string `json:"productName"`
	Category     string `json:"category"`
	Description  string `json:"description"`
	ResourceType string `json:"resourceType"`
	RegionScope  string `json:"regionScope"` // REGIONAL | ZONAL (topology.RegionScope)
	CrossAZ      bool   `json:"crossAz"`     // ZONAL only: cross-AZ replicas (HA) vs single-AZ
	Status       int    `json:"status"`
	OwnerTeam    string `json:"ownerTeam"`
}

// sku mirrors t_sku.
type sku struct {
	SKUCode     string            `json:"skuCode"`
	ProductCode string            `json:"productCode"`
	ChargeType  pricing.ChargeType `json:"chargeType"`
	SpecJSON    string            `json:"specJson"`
	Status      string            `json:"status"`
}

// catalogStore is the in-memory stand-in for the four catalogue tables. It is
// the single source of rules/promos handed to the pricing engine per request,
// mirroring how the production repository loads candidates for a quote.
type catalogStore struct {
	mu      sync.RWMutex
	products []product
	skus     []sku
	rules    []pricing.PricingRule
	promos   []pricing.Promotion
	engine   pricing.Engine
}

func newCatalogStore() *catalogStore {
	s := &catalogStore{}
	s.seed()
	return s
}

// seed mirrors sql/V2__seed_phase1_catalog.sql plus the phase-2 SCECI addition.
// The 7 sellable products of decision R-01 / adjudication C1+S1: SCVPC / SCECS
// / SCBS / SCOSS / SCRDS / SCMON / SCEIP. SCECI (弹性容器实例) lands in phase-2
// (M-7.1, 09-roadmap §4.3): a per-second-billed container instance fulfilled by
// the K8s driver (DriverK8s), distinct from SCECS's VM driver (DriverVM/_mock).
func (s *catalogStore) seed() {
	s.products = []product{
		{ProductCode: "scvpc", ProductName: "辰云专有网络", Category: "network", Description: "租户逻辑隔离网络,一切资源的网络边界", ResourceType: "vpc", RegionScope: "REGIONAL", Status: 2, OwnerTeam: "network-line"},
		{ProductCode: "scecs", ProductName: "辰云服务器", Category: "compute", Description: "云上虚拟服务器,一切资源的基础算力载体", ResourceType: "instance", RegionScope: "ZONAL", CrossAZ: true, Status: 2, OwnerTeam: "compute-line"},
		{ProductCode: "scbs", ProductName: "辰云块存储", Category: "storage", Description: "挂载云服务器的高性能云盘", ResourceType: "disk", RegionScope: "ZONAL", CrossAZ: false, Status: 2, OwnerTeam: "storage-line"},
		{ProductCode: "scoss", ProductName: "辰云对象存储", Category: "storage", Description: "RESTful 海量非结构化存储,S3 兼容生态锚点", ResourceType: "bucket", RegionScope: "REGIONAL", Status: 2, OwnerTeam: "storage-line"},
		{ProductCode: "scrds", ProductName: "辰云数据库MySQL版", Category: "database", Description: "托管 MySQL 关系型数据库,企业上云标配", ResourceType: "dbinstance", RegionScope: "ZONAL", CrossAZ: true, Status: 2, OwnerTeam: "data-line"},
		{ProductCode: "scmon", ProductName: "辰云监控", Category: "monitor", Description: "资源与自定义指标监控告警", ResourceType: "monitor", RegionScope: "REGIONAL", Status: 2, OwnerTeam: "platform-line"},
		{ProductCode: "sceip", ProductName: "弹性公网IP", Category: "network", Description: "可独立购买与动态绑定的公网地址", ResourceType: "eip", RegionScope: "ZONAL", CrossAZ: false, Status: 2, OwnerTeam: "network-line"},
		// SCECI 弹性容器实例 (M-7.1, 09 §4.3 M-7, 06 §4.2 four-component pattern).
		// ZONAL: a container pod lands on a node in one AZ at create time (the
		// scheduler binds it to the AZ whose node has free capacity). Per-second
		// postpaid billing — the cheapest form for bursty/ephemeral compute, the
		// "弹性" partner to SCECS's VM 旗舰 (09 §3.2 D-03). Fulfilled by DriverK8s,
		// bound in provision.Registry (rc-eci controller), NOT the VM driver.
		{ProductCode: "sceci", ProductName: "弹性容器实例", Category: "compute", Description: "秒级拉起的容器实例,按秒计费,弹性计算的轻量搭档", ResourceType: "eci", RegionScope: "ZONAL", CrossAZ: false, Status: 2, OwnerTeam: "compute-line"},
		// SCLB 负载均衡 (M-7.2, P0): REGIONAL — a load balancer spans AZs (it is
		// the cross-AZ entry point). POSTPAID by usage (LCU + traffic). DriverK8s:
		// APISIX (L7) + LVS/IPVS (L4) abstracted as one CR (09 §4.2).
		{ProductCode: "sclb", ProductName: "辰云负载均衡", Category: "network", Description: "四层/七层负载均衡,跨可用区流量入口", ResourceType: "slb", RegionScope: "REGIONAL", CrossAZ: true, Status: 2, OwnerTeam: "network-line"},
		// SCAS 弹性伸缩 (M-7.3, P2): REGIONAL scaling group — the policy layer
		// over HPA/VPA/CA (06 §2.6). POSTPAID management fee; managed ECS/ECI
		// bill separately. DriverK8s.
		{ProductCode: "scas", ProductName: "弹性伸缩", Category: "management", Description: "伸缩组策略引擎,基于 ECS/ECI 自动扩缩容", ResourceType: "scalinggroup", RegionScope: "REGIONAL", CrossAZ: false, Status: 2, OwnerTeam: "compute-line"},
		// SCBACKUP 云备份 (M-7.4, P2): REGIONAL backup policy — scheduled snapshot
		// + cross-AZ copy. POSTPAID by stored capacity. Retention enforced (06 §4.1).
		{ProductCode: "scbackup", ProductName: "云备份", Category: "storage", Description: "定时快照与跨可用区备份策略,保留期自动清理", ResourceType: "backuppolicy", RegionScope: "REGIONAL", CrossAZ: false, Status: 2, OwnerTeam: "storage-line"},
		// SCREDIS 托管 Redis (M-7.5, P1, 09 §4.2 managed/middleware productization):
		// ZONAL with cross-AZ HA replica (master+replica across AZs) — the platform's
		// own redis-cluster ops (M-6.2a) turned into a managed product. Both prepay
		// and postpay (managed DBs offer both, like scrds). DriverK8s.
		{ProductCode: "scredis", ProductName: "辰云数据库Redis版", Category: "database", Description: "托管 Redis,主备跨可用区,平台运维经验产品化", ResourceType: "redisinstance", RegionScope: "ZONAL", CrossAZ: true, Status: 2, OwnerTeam: "data-line"},
		// SCKAFKA 托管 Kafka (M-7.5, P1, 09 §4.2 managed/middleware productization):
		// ZONAL with cross-AZ HA (brokers across AZs, min.insync.replicas=2 tolerates
		// one AZ loss) — the platform's own kafka-kraft ops (M-6.2a) productized.
		// Both prepay and postpay (managed middleware, like scredis). DriverK8s.
		{ProductCode: "sckafka", ProductName: "辰云消息队列Kafka版", Category: "middleware", Description: "托管 Kafka,跨可用区 broker,平台运维经验产品化", ResourceType: "kafkainstance", RegionScope: "ZONAL", CrossAZ: true, Status: 2, OwnerTeam: "data-line"},
		// SCLOG 日志服务 (M-7.5, P1, 09 §4.2): REGIONAL ingestion + storage
		// (Vector collect + ClickHouse store, multi-tenant topic/table). Cross-AZ
		// storage is a replica flag, not a placement constraint. DriverK8s.
		{ProductCode: "sclog", ProductName: "辰云日志服务", Category: "middleware", Description: "日志采集与存储,Vector+ClickHouse,多租户隔离", ResourceType: "loginstance", RegionScope: "REGIONAL", CrossAZ: false, Status: 2, OwnerTeam: "data-line"},
	}

	s.skus = []sku{
		{SKUCode: "scecs.s2.small.prepaid", ProductCode: "scecs", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"cpu":1,"mem_gb":2}`, Status: "1"},
		{SKUCode: "scecs.s2.small.postpaid", ProductCode: "scecs", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"cpu":1,"mem_gb":2}`, Status: "1"},
		{SKUCode: "scecs.s2.large.prepaid", ProductCode: "scecs", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"cpu":2,"mem_gb":4}`, Status: "1"},
		{SKUCode: "scecs.s2.large.postpaid", ProductCode: "scecs", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"cpu":2,"mem_gb":4}`, Status: "1"},
		{SKUCode: "scecs.s2.xlarge.prepaid", ProductCode: "scecs", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"cpu":4,"mem_gb":8}`, Status: "1"},
		{SKUCode: "scecs.s2.xlarge.postpaid", ProductCode: "scecs", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"cpu":4,"mem_gb":8}`, Status: "1"},
		{SKUCode: "scbs.essd.prepaid", ProductCode: "scbs", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"disk_type":"essd","min_gb":20}`, Status: "1"},
		{SKUCode: "scbs.essd.postpaid", ProductCode: "scbs", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"disk_type":"essd","min_gb":20}`, Status: "1"},
		{SKUCode: "scoss.standard.postpaid", ProductCode: "scoss", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"storage_class":"standard"}`, Status: "1"},
		{SKUCode: "scrds.mysql8.small.prepaid", ProductCode: "scrds", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"engine":"mysql","version":"8.0","cpu":2,"mem_gb":4,"ha":true}`, Status: "1"},
		{SKUCode: "scrds.mysql8.small.postpaid", ProductCode: "scrds", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"engine":"mysql","version":"8.0","cpu":2,"mem_gb":4,"ha":true}`, Status: "1"},
		{SKUCode: "sceip.bandwidth.prepaid", ProductCode: "sceip", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"billing":"bandwidth","mbps":5}`, Status: "1"},
		{SKUCode: "sceip.traffic.postpaid", ProductCode: "sceip", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"billing":"traffic"}`, Status: "1"},
		{SKUCode: "scvpc.standard.postpaid", ProductCode: "scvpc", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"tier":"standard"}`, Status: "1"},
		{SKUCode: "scmon.basic.postpaid", ProductCode: "scmon", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"granularity_s":60}`, Status: "1"},
		// SCECI 弹性容器实例 (M-7.1): per-second POSTPAID only — containers are
		// the elastic/bursty form, billed by the second, no prepaid variant (a
		// reserved container would just be a VM). cpu/mem_gb mirror SCECS specs.
		{SKUCode: "sceci.c2.small.postpaid", ProductCode: "sceci", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"cpu":1,"mem_gb":2}`, Status: "1"},
		{SKUCode: "sceci.c2.large.postpaid", ProductCode: "sceci", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"cpu":2,"mem_gb":4}`, Status: "1"},
		{SKUCode: "sceci.c2.xlarge.postpaid", ProductCode: "sceci", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"cpu":4,"mem_gb":8}`, Status: "1"},
		// SCLB 负载均衡 (M-7.2): POSTPAID by usage (L7 = APISIX by QPS, L4 = conn).
		{SKUCode: "sclb.l1.small.postpaid", ProductCode: "sclb", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"type":"l7","max_qps":100}`, Status: "1"},
		{SKUCode: "sclb.l1.large.postpaid", ProductCode: "sclb", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"type":"l7","max_qps":5000}`, Status: "1"},
		{SKUCode: "sclb.l4.conn.postpaid", ProductCode: "sclb", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"type":"l4","max_conn":100000}`, Status: "1"},
		// SCAS 弹性伸缩 (M-7.3): POSTPAID management fee per scaling-group-hour.
		{SKUCode: "scas.standard.postpaid", ProductCode: "scas", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"managed_type":"ecs"}`, Status: "1"},
		{SKUCode: "scas.eci.postpaid", ProductCode: "scas", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"managed_type":"eci"}`, Status: "1"},
		// SCBACKUP 云备份 (M-7.4): POSTPAID by stored capacity.
		{SKUCode: "scbackup.standard.postpaid", ProductCode: "scbackup", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"tier":"standard"}`, Status: "1"},
		{SKUCode: "scbackup.crossaz.postpaid", ProductCode: "scbackup", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"tier":"crossaz","cross_az":true}`, Status: "1"},
		// SCREDIS 托管 Redis (M-7.5): both prepay and postpay (managed DB).
		{SKUCode: "scredis.redis.small.prepaid", ProductCode: "scredis", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"engine":"redis","version":"7.0","mem_gb":1,"ha":true}`, Status: "1"},
		{SKUCode: "scredis.redis.small.postpaid", ProductCode: "scredis", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"engine":"redis","version":"7.0","mem_gb":1,"ha":true}`, Status: "1"},
		{SKUCode: "scredis.redis.large.prepaid", ProductCode: "scredis", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"engine":"redis","version":"7.0","mem_gb":4,"ha":true}`, Status: "1"},
		{SKUCode: "scredis.redis.large.postpaid", ProductCode: "scredis", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"engine":"redis","version":"7.0","mem_gb":4,"ha":true}`, Status: "1"},
		// SCKAFKA 托管 Kafka (M-7.5): both prepay and postpay (managed middleware).
		{SKUCode: "sckafka.kafka.standard.prepaid", ProductCode: "sckafka", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"engine":"kafka","version":"3.7","broker_count":3,"cross_az":true}`, Status: "1"},
		{SKUCode: "sckafka.kafka.standard.postpaid", ProductCode: "sckafka", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"engine":"kafka","version":"3.7","broker_count":3,"cross_az":true}`, Status: "1"},
		{SKUCode: "sckafka.kafka.large.prepaid", ProductCode: "sckafka", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"engine":"kafka","version":"3.7","broker_count":5,"cross_az":true}`, Status: "1"},
		{SKUCode: "sckafka.kafka.large.postpaid", ProductCode: "sckafka", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"engine":"kafka","version":"3.7","broker_count":5,"cross_az":true}`, Status: "1"},
		// SCLOG 日志服务 (M-7.5): REGIONAL, postpaid by ingestion + storage.
		{SKUCode: "sclog.log.standard.postpaid", ProductCode: "sclog", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"tier":"standard","retention_days":7,"storage_gb":50}`, Status: "1"},
		{SKUCode: "sclog.log.pro.postpaid", ProductCode: "sclog", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"tier":"pro","retention_days":30,"storage_gb":500,"cross_az":true}`, Status: "1"},
	}

	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.rules = []pricing.PricingRule{
		// SCECS 包年包月 (元/月)
		{RuleID: 1, SKUCode: "scecs.s2.small.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("90"), EffectiveFrom: from},
		{RuleID: 2, SKUCode: "scecs.s2.large.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("180"), EffectiveFrom: from},
		{RuleID: 3, SKUCode: "scecs.s2.xlarge.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("360"), EffectiveFrom: from},
		{RuleID: 4, SKUCode: "scecs.s2.large.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("150"), CustomerLevel: "ENTERPRISE", EffectiveFrom: from},
		{RuleID: 5, SKUCode: "scecs.s2.large.prepaid", RegionID: "cn-east-1", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("200"), EffectiveFrom: from},
		// SCECS 按量 (元/小时)
		{RuleID: 6, SKUCode: "scecs.s2.small.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.125"), EffectiveFrom: from},
		{RuleID: 7, SKUCode: "scecs.s2.large.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.25"), EffectiveFrom: from},
		{RuleID: 8, SKUCode: "scecs.s2.xlarge.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.5"), EffectiveFrom: from},
		// SCBS
		{RuleID: 9, SKUCode: "scbs.essd.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("1"), EffectiveFrom: from},
		{RuleID: 10, SKUCode: "scbs.essd.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.0014"), EffectiveFrom: from},
		// SCOSS
		{RuleID: 11, SKUCode: "scoss.standard.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.00017"), EffectiveFrom: from},
		// SCRDS
		{RuleID: 12, SKUCode: "scrds.mysql8.small.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("420"), EffectiveFrom: from},
		{RuleID: 13, SKUCode: "scrds.mysql8.small.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.7"), EffectiveFrom: from},
		// SCEIP
		{RuleID: 14, SKUCode: "sceip.bandwidth.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("23"), EffectiveFrom: from},
		{RuleID: 15, SKUCode: "sceip.traffic.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.8"), EffectiveFrom: from},
		// SCVPC / SCMON 基础档零费率
		{RuleID: 16, SKUCode: "scvpc.standard.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0"), EffectiveFrom: from},
		{RuleID: 17, SKUCode: "scmon.basic.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0"), EffectiveFrom: from},
		// SCECI 弹性容器实例 (M-7.1): per-second billing — DurationUnit SECOND,
		// the unit M-4.2 reserved for exactly this (09 §4.2). The list price is
		// per-second; the pricing engine multiplies list × quantity × duration,
		// so a 3600-second quote = list × 3600, reconciling to the hourly rate.
		{RuleID: 18, SKUCode: "sceci.c2.small.postpaid", RegionID: "*", DurationUnit: pricing.DurationSecond, ListPrice: pricing.MustParseAmount("0.000035"), EffectiveFrom: from},
		{RuleID: 19, SKUCode: "sceci.c2.large.postpaid", RegionID: "*", DurationUnit: pricing.DurationSecond, ListPrice: pricing.MustParseAmount("0.00007"), EffectiveFrom: from},
		{RuleID: 20, SKUCode: "sceci.c2.xlarge.postpaid", RegionID: "*", DurationUnit: pricing.DurationSecond, ListPrice: pricing.MustParseAmount("0.00014"), EffectiveFrom: from},
		// SCLB 负载均衡 (M-7.2): per-hour postpaid by usage tier.
		{RuleID: 21, SKUCode: "sclb.l1.small.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.06"), EffectiveFrom: from},
		{RuleID: 22, SKUCode: "sclb.l1.large.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.3"), EffectiveFrom: from},
		{RuleID: 23, SKUCode: "sclb.l4.conn.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.12"), EffectiveFrom: from},
		// SCAS 弹性伸缩 (M-7.3): per-hour management fee (managed instances bill separately).
		{RuleID: 24, SKUCode: "scas.standard.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.02"), EffectiveFrom: from},
		{RuleID: 25, SKUCode: "scas.eci.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.02"), EffectiveFrom: from},
		// SCBACKUP 云备份 (M-7.4): per-hour by stored capacity tier.
		{RuleID: 26, SKUCode: "scbackup.standard.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.0007"), EffectiveFrom: from},
		{RuleID: 27, SKUCode: "scbackup.crossaz.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.0014"), EffectiveFrom: from},
		// SCREDIS 托管 Redis (M-7.5): prepay 元/月 + postpay 元/小时 (managed DB, both forms).
		{RuleID: 28, SKUCode: "scredis.redis.small.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("78"), EffectiveFrom: from},
		{RuleID: 29, SKUCode: "scredis.redis.small.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.16"), EffectiveFrom: from},
		{RuleID: 30, SKUCode: "scredis.redis.large.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("300"), EffectiveFrom: from},
		{RuleID: 31, SKUCode: "scredis.redis.large.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.62"), EffectiveFrom: from},
		// SCKAFKA 托管 Kafka (M-7.5): prepay 元/月 + postpay 元/小时 (managed middleware, both forms).
		{RuleID: 32, SKUCode: "sckafka.kafka.standard.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("450"), EffectiveFrom: from},
		{RuleID: 33, SKUCode: "sckafka.kafka.standard.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.85"), EffectiveFrom: from},
		{RuleID: 34, SKUCode: "sckafka.kafka.large.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("1200"), EffectiveFrom: from},
		{RuleID: 35, SKUCode: "sckafka.kafka.large.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("2.4"), EffectiveFrom: from},
		// SCLOG 日志服务 (M-7.5): postpaid by storage-hour (ingestion_gb metered by USAGE, 05§4.1).
		{RuleID: 36, SKUCode: "sclog.log.standard.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.005"), EffectiveFrom: from},
		{RuleID: 37, SKUCode: "sclog.log.pro.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.02"), EffectiveFrom: from},
	}

	to := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	s.promos = []pricing.Promotion{
		{PromoID: "promo-newuser-2026", PromoType: pricing.PromoDiscountRate, ScopeType: "ORDER", RateBasisPoints: 3000, UserTag: "new", StartAt: from, EndAt: to},
		{PromoID: "promo-ecs-annual", PromoType: pricing.PromoDiscountRate, ScopeType: "PRODUCT", ScopeRef: "scecs", RateBasisPoints: 8500, StartAt: from, EndAt: to},
	}
}

// quoteRequest is the 询价 API body. productCode + chargeType + duration are
// the user-facing inputs; specCode selects the SKU, and quantity defaults to 1.
// ZoneID is required for ZONAL products (M-6): a VM is pinned to one AZ at
// create time, so the quote must carry the AZ so placement can be validated and
// the resource_id built with the right zone context.
type quoteRequest struct {
	ProductCode string `json:"productCode"`
	SpecCode    string `json:"specCode"`    // SKU code, e.g. scecs.s2.large.prepaid
	ChargeType  string `json:"chargeType"`  // PREPAID / POSTPAID
	Duration    int64  `json:"duration"`     // months for PREPAID; ignored for POSTPAID
	Quantity    int64  `json:"quantity"`     // instances; defaults to 1
	RegionID    string `json:"regionId"`    // optional; "" → wildcard rule
	ZoneID      string `json:"zoneId"`      // optional REGIONAL; required ZONAL (M-6, 00§4.4)
	CustomerLevel string `json:"customerLevel"`
	UserTag     string `json:"userTag"`      // "new" for first-purchase promo
}

func (s *catalogStore) handleQuote(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountIDFrom(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req quoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed body")
		return
	}
	if req.ProductCode == "" {
		writeErr(w, "Common.InvalidParameter", 400, "productCode is required")
		return
	}
	if req.SpecCode == "" {
		writeErr(w, "Common.InvalidParameter", 400, "specCode is required")
		return
	}

	// Resolve the SKU from the registry so the charge type and spec come from
	// the catalogue, not the caller's claim. A specCode that is not in the
	// catalogue cannot be quoted.
	s.mu.RLock()
	var matched *sku
	for i := range s.skus {
		if s.skus[i].SKUCode == req.SpecCode && s.skus[i].ProductCode == req.ProductCode {
			matched = &s.skus[i]
			break
		}
	}
	rules := s.rules
	promos := s.promos
	// Resolve the product record too, so the RegionScope can be enforced. The
	// SKU loop already guarantees the productCode is one we sell, but the scope
	// (REGIONAL/ZONAL) lives on the product, not the SKU — the same SKU code
	// cannot be both pinned and spread.
	var matchedProduct *product
	for i := range s.products {
		if s.products[i].ProductCode == req.ProductCode {
			matchedProduct = &s.products[i]
			break
		}
	}
	s.mu.RUnlock()
	if matched == nil {
		writeErr(w, "Catalog.SKUNotFound", 404, "规格不存在或与产品不匹配")
		return
	}

	// Placement gate (M-6, 00§4.4): a ZONAL product pins to one AZ at create
	// time, so its quote must carry a zoneId that is a valid AZ in the quote's
	// region. REGIONAL products ignore zoneId (they spread). Rejecting a
	// missing/invalid zone at 询价 — before any order is created — is how a
	// mis-placed ZONAL resource is stopped at the cheapest checkpoint.
	scope := topology.RegionScope(matchedProduct.RegionScope)
	if !scope.Valid() {
		writeErr(w, "Catalog.InvalidRegionScope", 500,
			"product "+req.ProductCode+" has invalid regionScope "+matchedProduct.RegionScope)
		return
	}
	if scope.Zonal() {
		if req.ZoneID == "" {
			writeErr(w, "Catalog.ZoneRequired", 400,
				"ZONAL product "+req.ProductCode+" requires zoneId")
			return
		}
		// Validate the AZ name shape and that it belongs to the quoted region.
		az, err := topology.ParseAZName(req.ZoneID)
		if err != nil {
			writeErr(w, "Catalog.InvalidZone", 400, err.Error())
			return
		}
		if req.RegionID != "" && az.Region != req.RegionID {
			writeErr(w, "Catalog.ZoneRegionMismatch", 400,
				"zone "+req.ZoneID+" is not in region "+req.RegionID)
			return
		}
	}

	ct := pricing.ChargeType(req.ChargeType)
	if ct == "" {
		ct = matched.ChargeType
	}
	if req.Quantity == 0 {
		req.Quantity = 1
	}

	// Duration unit follows the catalogue rule, not a hardcoded assumption.
	// Historically postpaid was always HOUR and prepaid MONTH, but the rule is
	// the authority — and SCECI (M-7.1) breaks the assumption: its postpaid
	// rule is per-SECOND (09 §4.2). Deriving the unit from the matched rule
	// means a new billing granularity is a catalogue row, not a code change,
	// and selectRule's DurationUnit match (pricing.go) keeps working. The
	// engine multiplies list × duration for PREPAID only, so the duration
	// value is irrelevant for postpaid (it is carried through to the response
	// purely as the period the user asked about).
	dur := req.Duration
	var durUnit pricing.DurationUnit
	if ct == pricing.ChargePrepaid {
		durUnit = pricing.DurationMonth
		if dur <= 0 {
			dur = 1
		}
	} else {
		durUnit = durationUnitForSKU(rules, req.SpecCode, req.RegionID, req.CustomerLevel, time.Now())
	}

	preq := pricing.Request{
		AccountID:      acct,
		ProductCode:    req.ProductCode,
		SKUCode:        req.SpecCode,
		RegionID:       req.RegionID,
		ChargeType:     ct,
		Duration:       dur,
		DurationUnit:   durUnit,
		Quantity:       req.Quantity,
		CustomerLevel:  req.CustomerLevel,
		UserTag:        req.UserTag,
		At:             time.Now(),
	}

	// The real pricing engine: 目录价 → 促销折扣 → 代金券. Phase-1 catalog
	// quotes carry no coupons (vouchers are presented at checkout, not 询价);
	// an empty coupon slice is the correct input.
	res, err := s.engine.Calculate(preq, rules, promos, nil)
	if err != nil {
		writeErr(w, "Catalog.QuoteFailed", 400, err.Error())
		return
	}
	writeJSON(w, "OK", resultToMap(res, ct, dur, durUnit, req.Quantity))}

func (s *catalogStore) handleProducts(w http.ResponseWriter, r *http.Request) {
	if _, ok := accountIDFrom(w, r); !ok {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0, len(s.products))
	for _, p := range s.products {
		out = append(out, productToMap(p))
	}
	writeJSON(w, "OK", out)
}

func (s *catalogStore) handleSKUs(w http.ResponseWriter, r *http.Request) {
	if _, ok := accountIDFrom(w, r); !ok {
		return
	}
	productCode := r.URL.Query().Get("productCode")
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0)
	for _, sk := range s.skus {
		if productCode != "" && sk.ProductCode != productCode {
			continue
		}
		out = append(out, skuToMap(sk))
	}
	writeJSON(w, "OK", out)
}

// handlePlacement returns the placement contract for a product: its scope
// (REGIONAL/ZONAL), whether it carries cross-AZ replicas, and whether a zoneId
// is required at create time. The console uses this to render the AZ picker in
// the purchase wizard (a ZONAL product shows the zone dropdown, a REGIONAL one
// does not), and the quote path enforces the same contract server-side.
// (M-6, 00§4.4.)
func (s *catalogStore) handlePlacement(w http.ResponseWriter, r *http.Request) {
	if _, ok := accountIDFrom(w, r); !ok {
		return
	}
	productCode := r.URL.Query().Get("productCode")
	if productCode == "" {
		writeErr(w, "Common.InvalidParameter", 400, "productCode is required")
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var matched *product
	for i := range s.products {
		if s.products[i].ProductCode == productCode {
			matched = &s.products[i]
			break
		}
	}
	if matched == nil {
		writeErr(w, "Catalog.ProductNotFound", 404, "产品不存在")
		return
	}
	scope := topology.RegionScope(matched.RegionScope)
	writeJSON(w, "OK", map[string]any{
		"productCode":  matched.ProductCode,
		"regionScope": matched.RegionScope,
		"zonal":        scope.Zonal(),
		"crossAz":      matched.CrossAZ,
		"zoneRequired": scope.Zonal(),
	})
}

// durationUnitForSKU returns the DurationUnit of the pricing rule that would
// match a postpaid quote for this SKU, defaulting to HOUR (the phase-1 norm)
// when no rule is found yet. The catalogue is the authority for billing
// granularity: SCECI postpaid is per-SECOND (M-7.1, 09 §4.2), while every other
// postpaid product is per-HOUR. Reading it from the rule means selectRule's
// DurationUnit match (pricing.go) succeeds without the quote path hardcoding
// the unit per product.
func durationUnitForSKU(rules []pricing.PricingRule, skuCode, regionID, customerLevel string, at time.Time) pricing.DurationUnit {
	var best pricing.PricingRule
	found := false
	for _, r := range rules {
		if r.SKUCode != skuCode || !r.Active(at) {
			continue
		}
		if r.RegionID != "*" && r.RegionID != "" && r.RegionID != regionID {
			continue
		}
		if r.CustomerLevel != "" && r.CustomerLevel != customerLevel {
			continue
		}
		if !found || r.Specificity() > best.Specificity() {
			best, found = r, true
		}
	}
	if found && best.DurationUnit != "" {
		return best.DurationUnit
	}
	return pricing.DurationHour
}

func resultToMap(res pricing.Result, ct pricing.ChargeType, dur int64, unit pricing.DurationUnit, qty int64) map[string]any {
	coupons := make([]map[string]any, 0, len(res.AppliedCoupons))
	for _, c := range res.AppliedCoupons {
		coupons = append(coupons, map[string]any{"couponId": c.CouponID, "amount": c.Amount.String()})
	}
	return map[string]any{
		"listAmount":     res.ListAmount.String(),
		"promoAmount":    res.PromoAmount.String(),
		"couponAmount":   res.CouponAmount.String(),
		"payableAmount":  res.PayableAmount.String(),
		"appliedPromoId": res.AppliedPromoID,
		"appliedCoupons": coupons,
		"ruleId":         res.RuleID,
		"chargeType":     string(ct),
		"duration":       dur,
		"durationUnit":   string(unit),
		"quantity":       qty,
	}
}

func productToMap(p product) map[string]any {
	return map[string]any{
		"productCode": p.ProductCode, "productName": p.ProductName,
		"category": p.Category, "description": p.Description,
		"resourceType": p.ResourceType, "regionScope": p.RegionScope,
		"crossAz": p.CrossAZ, "status": p.Status, "ownerTeam": p.OwnerTeam,
	}
}

func skuToMap(sk sku) map[string]any {
	return map[string]any{
		"skuCode": sk.SKUCode, "productCode": sk.ProductCode,
		"chargeType": string(sk.ChargeType), "specJson": sk.SpecJSON,
		"status": sk.Status,
	}
}

func accountIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.Header.Get(accountIDHeader)
	if raw == "" {
		writeErr(w, "Common.MissingAccountId", 403, "X-Sc-Account-Id header is required")
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed X-Sc-Account-Id")
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, code string, data any) {
	rid := w.Header().Get("X-Sc-TraceId")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Data": data})
}

func writeErr(w http.ResponseWriter, code string, status int, msg string) {
	rid := w.Header().Get("X-Sc-TraceId")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Message": msg})
}

func requestIDMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Sc-TraceId")
		if id == "" {
			id = fmt.Sprintf("catalog-%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Sc-TraceId", id)
		h.ServeHTTP(w, r)
	})
}

func recoverMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic", "rec", rec, "path", r.URL.Path)
				writeErr(w, "Common.InternalError", 500, "internal error")
			}
		}()
		h.ServeHTTP(w, r)
	})
}

func main() {
	addr := flag.String("http", ":9207", "HTTP listen address")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	store := newCatalogStore()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("POST /api/v1/catalog/quote", store.handleQuote)
	mux.HandleFunc("GET /api/v1/catalog/products", store.handleProducts)
	mux.HandleFunc("GET /api/v1/catalog/skus", store.handleSKUs)
	mux.HandleFunc("GET /api/v1/catalog/placement", store.handlePlacement)

	srv := &http.Server{Addr: *addr, Handler: recoverMiddleware(requestIDMiddleware(mux)), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("svc-catalog listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	slog.Info("shutdown signal received, draining")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
}
