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
// Routes:
//
//	POST  /api/v1/catalog/quote    — 询价: runs the real pricing engine, returns pricing.Result (account required)
//	GET   /api/v1/catalog/products — list seeded products (anonymous-safe: public marketing data)
//	GET   /api/v1/catalog/skus     — list SKUs, filter by ?productCode= (anonymous-safe)
//	GET   /api/v1/catalog/placement — placement contract (scope/crossAz/zoneRequired), M-6 (anonymous-safe)
//	GET   /api/v1/catalog/regions  — region/zone metadata for console region pickers (anonymous-safe)
//	GET   /api/v1/catalog/region-topology — M-8 两地三中心 plan + replication RPO verdicts (anonymous-safe)
//	GET   /api/v1/catalog/images   — public image list, filter by ?productCode= (anonymous-safe)
//	/healthz, /readyz
//
// stdlib-HTTP service. Envelope {RequestId,Code,Data} (03§9.3). In-memory store
// (MySQL t_product/t_sku/t_pricing_rule/t_promo_policy in production). The seed
// mirrors sql/V2__seed_phase1_catalog.sql — when the SQL seed changes, the
// catalogue() below changes with it (the server's own tests enforce the same).
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

	"github.com/qifalab/euler-platform/multiregion"
	"github.com/qifalab/euler-platform/pricing"
	"github.com/qifalab/euler-platform/topology"
)

const accountIDHeader = "X-Euler-Account-Id"

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

// zone is one availability zone inside a region (00§4.1). The zone id follows
// the {region}-{letter} convention (identifier.AZName); ZoneName is the
// display label the console renders.
type zone struct {
	ZoneID   string `json:"zoneId"`
	ZoneName string `json:"zoneName"`
}

// region is the sellable region metadata the console's region pickers render
// (console-base top bar, every BuyWizard's 地域/可用区 dropdown). The catalogue
// is the authority for "which regions can a resource be placed in" — the
// placement contract (M-6) is per-product, but the region/zone inventory is
// global, so it lives here alongside it. P2 shape: dual-AZ per region
// (topology.NewTopology validates the same convention).
type region struct {
	RegionID   string `json:"regionId"`
	RegionName string `json:"regionName"`
	Zones      []zone `json:"zones"`
}

// image is a public OS image a compute product (EUECS) can boot from. The
// catalogue owns it for the same reason it owns SKUs: the console must not
// invent bootable images client-side — an image id that provisioning does not
// know would fail at fulfilment time, which is the most expensive place to
// learn the console was guessing.
type image struct {
	ImageID     string `json:"imageId"`
	Name        string `json:"name"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	ProductCode string `json:"productCode"`
	Status      int    `json:"status"`
}

// category is the marketing taxonomy row (t_category): it groups products on
// the site's category grid and the console's product nav. Display names and
// doc links live server-side so re-branding a category is a data edit, never a
// frontend release. Code matches product.Category.
type category struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Link        string `json:"link"`
}

// catalogStore is the in-memory stand-in for the four catalogue tables. It is
// the single source of rules/promos handed to the pricing engine per request,
// mirroring how the production repository loads candidates for a quote.
type catalogStore struct {
	mu         sync.RWMutex
	products   []product
	skus       []sku
	regions    []region
	images     []image
	categories []category
	rules      []pricing.PricingRule
	promos     []pricing.Promotion
	engine     pricing.Engine
}

func newCatalogStore() *catalogStore {
	s := &catalogStore{}
	s.seed()
	return s
}

// seed fills the in-memory store. The DB-backed store takes the quote-path
// tables from trade_db (repo.go) and serves the inventory lists (regions/
// images/categories) from the same seed in both modes.
func (s *catalogStore) seed() {
	s.products = seedProducts()
	s.skus = seedSKUs()
	s.rules = seedRules()
	s.promos = seedPromos()
	s.regions = seedRegions()
	s.images = seedImages()
	s.categories = seedCategories()
}

// newCatalogStoreWith builds a store from an already-loaded catalogue.
func newCatalogStoreWith(data catalogData) *catalogStore {
	return &catalogStore{
		products:   data.Products,
		skus:       data.SKUs,
		rules:      data.Rules,
		promos:     data.Promos,
		regions:    seedRegions(),
		images:     seedImages(),
		categories: seedCategories(),
	}
}

// seedProducts lists the sellable products of decision R-01 / adjudication
// C1+S1: EUVPC / EUECS / EUBS / EUOSS / EURDS / EUMON / EUEIP. EUECI (弹性容器
// 实例) lands in phase-2 (M-7.1, 09-roadmap §4.3): a per-second-billed container
// instance fulfilled by the K8s driver (DriverK8s), distinct from EUECS's VM
// driver (DriverVM/_mock).
func seedProducts() []product {
	return []product{
		{ProductCode: "euvpc", ProductName: "辰云专有网络", Category: "network", Description: "租户逻辑隔离网络,一切资源的网络边界", ResourceType: "vpc", RegionScope: "REGIONAL", Status: 2, OwnerTeam: "network-line"},
		{ProductCode: "euecs", ProductName: "辰云服务器", Category: "compute", Description: "云上虚拟服务器,一切资源的基础算力载体", ResourceType: "instance", RegionScope: "ZONAL", CrossAZ: true, Status: 2, OwnerTeam: "compute-line"},
		{ProductCode: "eubs", ProductName: "辰云块存储", Category: "storage", Description: "挂载云服务器的高性能云盘", ResourceType: "disk", RegionScope: "ZONAL", CrossAZ: false, Status: 2, OwnerTeam: "storage-line"},
		{ProductCode: "euoss", ProductName: "辰云对象存储", Category: "storage", Description: "RESTful 海量非结构化存储,S3 兼容生态锚点", ResourceType: "bucket", RegionScope: "REGIONAL", Status: 2, OwnerTeam: "storage-line"},
		{ProductCode: "eurds", ProductName: "辰云数据库MySQL版", Category: "database", Description: "托管 MySQL 关系型数据库,企业上云标配", ResourceType: "dbinstance", RegionScope: "ZONAL", CrossAZ: true, Status: 2, OwnerTeam: "data-line"},
		{ProductCode: "eumon", ProductName: "辰云监控", Category: "monitor", Description: "资源与自定义指标监控告警", ResourceType: "monitor", RegionScope: "REGIONAL", Status: 2, OwnerTeam: "platform-line"},
		{ProductCode: "eueip", ProductName: "弹性公网IP", Category: "network", Description: "可独立购买与动态绑定的公网地址", ResourceType: "eip", RegionScope: "ZONAL", CrossAZ: false, Status: 2, OwnerTeam: "network-line"},
		// EUECI 弹性容器实例 (M-7.1, 09 §4.3 M-7, 06 §4.2 four-component pattern).
		// ZONAL: a container pod lands on a node in one AZ at create time (the
		// scheduler binds it to the AZ whose node has free capacity). Per-second
		// postpaid billing — the cheapest form for bursty/ephemeral compute, the
		// "弹性" partner to EUECS's VM 旗舰 (09 §3.2 D-03). Fulfilled by DriverK8s,
		// bound in provision.Registry (rc-eci controller), NOT the VM driver.
		{ProductCode: "eueci", ProductName: "弹性容器实例", Category: "compute", Description: "秒级拉起的容器实例,按秒计费,弹性计算的轻量搭档", ResourceType: "eci", RegionScope: "ZONAL", CrossAZ: false, Status: 2, OwnerTeam: "compute-line"},
		// EULB 负载均衡 (M-7.2, P0): REGIONAL — a load balancer spans AZs (it is
		// the cross-AZ entry point). POSTPAID by usage (LCU + traffic). DriverK8s:
		// APISIX (L7) + LVS/IPVS (L4) abstracted as one CR (09 §4.2).
		{ProductCode: "eulb", ProductName: "辰云负载均衡", Category: "network", Description: "四层/七层负载均衡,跨可用区流量入口", ResourceType: "slb", RegionScope: "REGIONAL", CrossAZ: true, Status: 2, OwnerTeam: "network-line"},
		// EUAS 弹性伸缩 (M-7.3, P2): REGIONAL scaling group — the policy layer
		// over HPA/VPA/CA (06 §2.6). POSTPAID management fee; managed ECS/ECI
		// bill separately. DriverK8s.
		{ProductCode: "euas", ProductName: "弹性伸缩", Category: "management", Description: "伸缩组策略引擎,基于 ECS/ECI 自动扩缩容", ResourceType: "scalinggroup", RegionScope: "REGIONAL", CrossAZ: false, Status: 2, OwnerTeam: "compute-line"},
		// EUBACKUP 云备份 (M-7.4, P2): REGIONAL backup policy — scheduled snapshot
		// + cross-AZ copy. POSTPAID by stored capacity. Retention enforced (06 §4.1).
		{ProductCode: "eubackup", ProductName: "云备份", Category: "storage", Description: "定时快照与跨可用区备份策略,保留期自动清理", ResourceType: "backuppolicy", RegionScope: "REGIONAL", CrossAZ: false, Status: 2, OwnerTeam: "storage-line"},
		// EUREDIS 托管 Redis (M-7.5, P1, 09 §4.2 managed/middleware productization):
		// ZONAL with cross-AZ HA replica (master+replica across AZs) — the platform's
		// own redis-cluster ops (M-6.2a) turned into a managed product. Both prepay
		// and postpay (managed DBs offer both, like eurds). DriverK8s.
		{ProductCode: "euredis", ProductName: "辰云数据库Redis版", Category: "database", Description: "托管 Redis,主备跨可用区,平台运维经验产品化", ResourceType: "redisinstance", RegionScope: "ZONAL", CrossAZ: true, Status: 2, OwnerTeam: "data-line"},
		// EUKAFKA 托管 Kafka (M-7.5, P1, 09 §4.2 managed/middleware productization):
		// ZONAL with cross-AZ HA (brokers across AZs, min.insync.replicas=2 tolerates
		// one AZ loss) — the platform's own kafka-kraft ops (M-6.2a) productized.
		// Both prepay and postpay (managed middleware, like euredis). DriverK8s.
		{ProductCode: "eukafka", ProductName: "辰云消息队列Kafka版", Category: "middleware", Description: "托管 Kafka,跨可用区 broker,平台运维经验产品化", ResourceType: "kafkainstance", RegionScope: "ZONAL", CrossAZ: true, Status: 2, OwnerTeam: "data-line"},
		// EULOG 日志服务 (M-7.5, P1, 09 §4.2): REGIONAL ingestion + storage
		// (Vector collect + ClickHouse store, multi-tenant topic/table). Cross-AZ
		// storage is a replica flag, not a placement constraint. DriverK8s.
		{ProductCode: "eulog", ProductName: "辰云日志服务", Category: "middleware", Description: "日志采集与存储,Vector+ClickHouse,多租户隔离", ResourceType: "loginstance", RegionScope: "REGIONAL", CrossAZ: false, Status: 2, OwnerTeam: "data-line"},
	}
}

// seedSKUs lists the sellable SKUs: one row per product × spec × charge form.
func seedSKUs() []sku {
	return []sku{
		{SKUCode: "euecs.s2.small.prepaid", ProductCode: "euecs", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"cpu":1,"mem_gb":2}`, Status: "1"},
		{SKUCode: "euecs.s2.small.postpaid", ProductCode: "euecs", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"cpu":1,"mem_gb":2}`, Status: "1"},
		{SKUCode: "euecs.s2.large.prepaid", ProductCode: "euecs", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"cpu":2,"mem_gb":4}`, Status: "1"},
		{SKUCode: "euecs.s2.large.postpaid", ProductCode: "euecs", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"cpu":2,"mem_gb":4}`, Status: "1"},
		{SKUCode: "euecs.s2.xlarge.prepaid", ProductCode: "euecs", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"cpu":4,"mem_gb":8}`, Status: "1"},
		{SKUCode: "euecs.s2.xlarge.postpaid", ProductCode: "euecs", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"cpu":4,"mem_gb":8}`, Status: "1"},
		{SKUCode: "eubs.essd.prepaid", ProductCode: "eubs", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"disk_type":"essd","min_gb":20}`, Status: "1"},
		{SKUCode: "eubs.essd.postpaid", ProductCode: "eubs", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"disk_type":"essd","min_gb":20}`, Status: "1"},
		{SKUCode: "euoss.standard.postpaid", ProductCode: "euoss", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"storage_class":"standard"}`, Status: "1"},
		{SKUCode: "eurds.mysql8.small.prepaid", ProductCode: "eurds", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"engine":"mysql","version":"8.0","cpu":2,"mem_gb":4,"ha":true}`, Status: "1"},
		{SKUCode: "eurds.mysql8.small.postpaid", ProductCode: "eurds", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"engine":"mysql","version":"8.0","cpu":2,"mem_gb":4,"ha":true}`, Status: "1"},
		{SKUCode: "eueip.bandwidth.prepaid", ProductCode: "eueip", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"billing":"bandwidth","mbps":5}`, Status: "1"},
		{SKUCode: "eueip.traffic.postpaid", ProductCode: "eueip", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"billing":"traffic"}`, Status: "1"},
		{SKUCode: "euvpc.standard.postpaid", ProductCode: "euvpc", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"tier":"standard"}`, Status: "1"},
		{SKUCode: "eumon.basic.postpaid", ProductCode: "eumon", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"granularity_s":60}`, Status: "1"},
		// EUECI 弹性容器实例 (M-7.1): per-second POSTPAID only — containers are
		// the elastic/bursty form, billed by the second, no prepaid variant (a
		// reserved container would just be a VM). cpu/mem_gb mirror EUECS specs.
		{SKUCode: "eueci.c2.small.postpaid", ProductCode: "eueci", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"cpu":1,"mem_gb":2}`, Status: "1"},
		{SKUCode: "eueci.c2.large.postpaid", ProductCode: "eueci", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"cpu":2,"mem_gb":4}`, Status: "1"},
		{SKUCode: "eueci.c2.xlarge.postpaid", ProductCode: "eueci", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"cpu":4,"mem_gb":8}`, Status: "1"},
		// EULB 负载均衡 (M-7.2): POSTPAID by usage (L7 = APISIX by QPS, L4 = conn).
		{SKUCode: "eulb.l1.small.postpaid", ProductCode: "eulb", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"type":"l7","max_qps":100}`, Status: "1"},
		{SKUCode: "eulb.l1.large.postpaid", ProductCode: "eulb", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"type":"l7","max_qps":5000}`, Status: "1"},
		{SKUCode: "eulb.l4.conn.postpaid", ProductCode: "eulb", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"type":"l4","max_conn":100000}`, Status: "1"},
		// EUAS 弹性伸缩 (M-7.3): POSTPAID management fee per scaling-group-hour.
		{SKUCode: "euas.standard.postpaid", ProductCode: "euas", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"managed_type":"ecs"}`, Status: "1"},
		{SKUCode: "euas.eci.postpaid", ProductCode: "euas", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"managed_type":"eci"}`, Status: "1"},
		// EUBACKUP 云备份 (M-7.4): POSTPAID by stored capacity.
		{SKUCode: "eubackup.standard.postpaid", ProductCode: "eubackup", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"tier":"standard"}`, Status: "1"},
		{SKUCode: "eubackup.crossaz.postpaid", ProductCode: "eubackup", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"tier":"crossaz","cross_az":true}`, Status: "1"},
		// EUREDIS 托管 Redis (M-7.5): both prepay and postpay (managed DB).
		{SKUCode: "euredis.redis.small.prepaid", ProductCode: "euredis", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"engine":"redis","version":"7.0","mem_gb":1,"ha":true}`, Status: "1"},
		{SKUCode: "euredis.redis.small.postpaid", ProductCode: "euredis", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"engine":"redis","version":"7.0","mem_gb":1,"ha":true}`, Status: "1"},
		{SKUCode: "euredis.redis.large.prepaid", ProductCode: "euredis", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"engine":"redis","version":"7.0","mem_gb":4,"ha":true}`, Status: "1"},
		{SKUCode: "euredis.redis.large.postpaid", ProductCode: "euredis", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"engine":"redis","version":"7.0","mem_gb":4,"ha":true}`, Status: "1"},
		// EUKAFKA 托管 Kafka (M-7.5): both prepay and postpay (managed middleware).
		{SKUCode: "eukafka.kafka.standard.prepaid", ProductCode: "eukafka", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"engine":"kafka","version":"3.7","broker_count":3,"cross_az":true}`, Status: "1"},
		{SKUCode: "eukafka.kafka.standard.postpaid", ProductCode: "eukafka", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"engine":"kafka","version":"3.7","broker_count":3,"cross_az":true}`, Status: "1"},
		{SKUCode: "eukafka.kafka.large.prepaid", ProductCode: "eukafka", ChargeType: pricing.ChargePrepaid, SpecJSON: `{"engine":"kafka","version":"3.7","broker_count":5,"cross_az":true}`, Status: "1"},
		{SKUCode: "eukafka.kafka.large.postpaid", ProductCode: "eukafka", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"engine":"kafka","version":"3.7","broker_count":5,"cross_az":true}`, Status: "1"},
		// EULOG 日志服务 (M-7.5): REGIONAL, postpaid by ingestion + storage.
		{SKUCode: "eulog.log.standard.postpaid", ProductCode: "eulog", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"tier":"standard","retention_days":7,"storage_gb":50}`, Status: "1"},
		{SKUCode: "eulog.log.pro.postpaid", ProductCode: "eulog", ChargeType: pricing.ChargePostpaid, SpecJSON: `{"tier":"pro","retention_days":30,"storage_gb":500,"cross_az":true}`, Status: "1"},
	}
}

// seedRules lists the pricing rules — append-only in the schema (01§12.3), so
// the seed is the initial append and every later change is a new row.
func seedRules() []pricing.PricingRule {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return []pricing.PricingRule{
		// EUECS 包年包月 (元/月)
		{RuleID: 1, SKUCode: "euecs.s2.small.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("90"), EffectiveFrom: from},
		{RuleID: 2, SKUCode: "euecs.s2.large.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("180"), EffectiveFrom: from},
		{RuleID: 3, SKUCode: "euecs.s2.xlarge.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("360"), EffectiveFrom: from},
		{RuleID: 4, SKUCode: "euecs.s2.large.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("150"), CustomerLevel: "ENTERPRISE", EffectiveFrom: from},
		{RuleID: 5, SKUCode: "euecs.s2.large.prepaid", RegionID: "cn-east-1", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("200"), EffectiveFrom: from},
		// EUECS 按量 (元/小时)
		{RuleID: 6, SKUCode: "euecs.s2.small.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.125"), EffectiveFrom: from},
		{RuleID: 7, SKUCode: "euecs.s2.large.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.25"), EffectiveFrom: from},
		{RuleID: 8, SKUCode: "euecs.s2.xlarge.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.5"), EffectiveFrom: from},
		// EUBS
		{RuleID: 9, SKUCode: "eubs.essd.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("1"), EffectiveFrom: from},
		{RuleID: 10, SKUCode: "eubs.essd.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.0014"), EffectiveFrom: from},
		// EUOSS
		{RuleID: 11, SKUCode: "euoss.standard.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.00017"), EffectiveFrom: from},
		// EURDS
		{RuleID: 12, SKUCode: "eurds.mysql8.small.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("420"), EffectiveFrom: from},
		{RuleID: 13, SKUCode: "eurds.mysql8.small.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.7"), EffectiveFrom: from},
		// EUEIP
		{RuleID: 14, SKUCode: "eueip.bandwidth.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("23"), EffectiveFrom: from},
		{RuleID: 15, SKUCode: "eueip.traffic.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.8"), EffectiveFrom: from},
		// EUVPC / EUMON 基础档零费率
		{RuleID: 16, SKUCode: "euvpc.standard.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0"), EffectiveFrom: from},
		{RuleID: 17, SKUCode: "eumon.basic.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0"), EffectiveFrom: from},
		// EUECI 弹性容器实例 (M-7.1): per-second billing — DurationUnit SECOND,
		// the unit M-4.2 reserved for exactly this (09 §4.2). The list price is
		// per-second; the pricing engine multiplies list × quantity × duration,
		// so a 3600-second quote = list × 3600, reconciling to the hourly rate.
		{RuleID: 18, SKUCode: "eueci.c2.small.postpaid", RegionID: "*", DurationUnit: pricing.DurationSecond, ListPrice: pricing.MustParseAmount("0.000035"), EffectiveFrom: from},
		{RuleID: 19, SKUCode: "eueci.c2.large.postpaid", RegionID: "*", DurationUnit: pricing.DurationSecond, ListPrice: pricing.MustParseAmount("0.00007"), EffectiveFrom: from},
		{RuleID: 20, SKUCode: "eueci.c2.xlarge.postpaid", RegionID: "*", DurationUnit: pricing.DurationSecond, ListPrice: pricing.MustParseAmount("0.00014"), EffectiveFrom: from},
		// EULB 负载均衡 (M-7.2): per-hour postpaid by usage tier.
		{RuleID: 21, SKUCode: "eulb.l1.small.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.06"), EffectiveFrom: from},
		{RuleID: 22, SKUCode: "eulb.l1.large.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.3"), EffectiveFrom: from},
		{RuleID: 23, SKUCode: "eulb.l4.conn.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.12"), EffectiveFrom: from},
		// EUAS 弹性伸缩 (M-7.3): per-hour management fee (managed instances bill separately).
		{RuleID: 24, SKUCode: "euas.standard.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.02"), EffectiveFrom: from},
		{RuleID: 25, SKUCode: "euas.eci.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.02"), EffectiveFrom: from},
		// EUBACKUP 云备份 (M-7.4): per-hour by stored capacity tier.
		{RuleID: 26, SKUCode: "eubackup.standard.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.0007"), EffectiveFrom: from},
		{RuleID: 27, SKUCode: "eubackup.crossaz.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.0014"), EffectiveFrom: from},
		// EUREDIS 托管 Redis (M-7.5): prepay 元/月 + postpay 元/小时 (managed DB, both forms).
		{RuleID: 28, SKUCode: "euredis.redis.small.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("78"), EffectiveFrom: from},
		{RuleID: 29, SKUCode: "euredis.redis.small.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.16"), EffectiveFrom: from},
		{RuleID: 30, SKUCode: "euredis.redis.large.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("300"), EffectiveFrom: from},
		{RuleID: 31, SKUCode: "euredis.redis.large.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.62"), EffectiveFrom: from},
		// EUKAFKA 托管 Kafka (M-7.5): prepay 元/月 + postpay 元/小时 (managed middleware, both forms).
		{RuleID: 32, SKUCode: "eukafka.kafka.standard.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("450"), EffectiveFrom: from},
		{RuleID: 33, SKUCode: "eukafka.kafka.standard.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.85"), EffectiveFrom: from},
		{RuleID: 34, SKUCode: "eukafka.kafka.large.prepaid", RegionID: "*", DurationUnit: pricing.DurationMonth, ListPrice: pricing.MustParseAmount("1200"), EffectiveFrom: from},
		{RuleID: 35, SKUCode: "eukafka.kafka.large.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("2.4"), EffectiveFrom: from},
		// EULOG 日志服务 (M-7.5): postpaid by storage-hour (ingestion_gb metered by USAGE, 05§4.1).
		{RuleID: 36, SKUCode: "eulog.log.standard.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.005"), EffectiveFrom: from},
		{RuleID: 37, SKUCode: "eulog.log.pro.postpaid", RegionID: "*", DurationUnit: pricing.DurationHour, ListPrice: pricing.MustParseAmount("0.02"), EffectiveFrom: from},
	}
}

// seedPromos lists the platform promotions (询价 takes the single best
// applicable one, 01§12.3 — promotions never stack).
func seedPromos() []pricing.Promotion {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	return []pricing.Promotion{
		{PromoID: "promo-newuser-2026", PromoType: pricing.PromoDiscountRate, ScopeType: "ORDER", RateBasisPoints: 3000, UserTag: "new", StartAt: from, EndAt: to},
		{PromoID: "promo-ecs-annual", PromoType: pricing.PromoDiscountRate, ScopeType: "PRODUCT", ScopeRef: "euecs", RateBasisPoints: 8500, StartAt: from, EndAt: to},
	}
}

// seedRegions lists the sellable region/zone inventory (00§4.1, P2 dual-AZ
// shape). There is no DDL table for region/zone inventory yet — platform
// metadata, served from this seed in both modes; the zone ids are validated by
// the same identifier.AZName convention the quote path enforces (M-6).
func seedRegions() []region {
	return []region{
		{RegionID: "cn-north-1", RegionName: "华北 1（北京）", Zones: []zone{
			{ZoneID: "cn-north-1-a", ZoneName: "华北 1 可用区 A"},
			{ZoneID: "cn-north-1-b", ZoneName: "华北 1 可用区 B"},
		}},
		{RegionID: "cn-east-1", RegionName: "华东 1（杭州）", Zones: []zone{
			{ZoneID: "cn-east-1-a", ZoneName: "华东 1 可用区 A"},
			{ZoneID: "cn-east-1-b", ZoneName: "华东 1 可用区 B"},
		}},
		{RegionID: "cn-south-1", RegionName: "华南 1（深圳）", Zones: []zone{
			{ZoneID: "cn-south-1-a", ZoneName: "华南 1 可用区 A"},
			{ZoneID: "cn-south-1-b", ZoneName: "华南 1 可用区 B"},
		}},
	}
}

// seedImages lists the public image inventory for compute products. Like the
// region inventory there is no DDL table yet; the seed is the inventory in
// both modes. Status 2 = on-sale, mirroring the product convention.
func seedImages() []image {
	return []image{
		{ImageID: "centos-7.9", Name: "CentOS 7.9 64位", OS: "linux", Arch: "x86_64", ProductCode: "euecs", Status: 2},
		{ImageID: "ubuntu-22.04", Name: "Ubuntu 22.04 64位", OS: "linux", Arch: "x86_64", ProductCode: "euecs", Status: 2},
		{ImageID: "debian-12", Name: "Debian 12 64位", OS: "linux", Arch: "x86_64", ProductCode: "euecs", Status: 2},
		{ImageID: "rocky-9", Name: "Rocky Linux 9 64位", OS: "linux", Arch: "x86_64", ProductCode: "euecs", Status: 2},
		{ImageID: "windows-2022", Name: "Windows Server 2022 数据中心版 64位", OS: "windows", Arch: "x86_64", ProductCode: "euecs", Status: 2},
	}
}

// seedCategories lists the marketing taxonomy (edited by the marketing
// back-office; no DDL table yet). Codes cover every Category value used by the
// product seed.
func seedCategories() []category {
	return []category{
		{Code: "compute", Name: "计算", Description: "云服务器、容器实例等基础算力", Link: "https://docs.eulercloud.cn/compute"},
		{Code: "storage", Name: "存储", Description: "对象存储、块存储与备份", Link: "https://docs.eulercloud.cn/storage"},
		{Code: "database", Name: "数据库", Description: "托管关系型与缓存数据库", Link: "https://docs.eulercloud.cn/database"},
		{Code: "network", Name: "网络", Description: "专有网络、负载均衡与公网接入", Link: "https://docs.eulercloud.cn/network"},
		{Code: "middleware", Name: "中间件", Description: "消息队列与日志服务", Link: "https://docs.eulercloud.cn/middleware"},
		{Code: "monitor", Name: "监控运维", Description: "指标监控与告警", Link: "https://docs.eulercloud.cn/monitor"},
		{Code: "management", Name: "管理与治理", Description: "弹性伸缩等资源编排能力", Link: "https://docs.eulercloud.cn/management"},
	}
}

// quoteRequest is the 询价 API body. productCode + chargeType + duration are
// the user-facing inputs; specCode selects the SKU, and quantity defaults to 1.
// ZoneID is required for ZONAL products (M-6): a VM is pinned to one AZ at
// create time, so the quote must carry the AZ so placement can be validated and
// the resource_id built with the right zone context.
type quoteRequest struct {
	ProductCode string `json:"productCode"`
	SpecCode    string `json:"specCode"`    // SKU code, e.g. euecs.s2.large.prepaid
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
	// the authority — and EUECI (M-7.1) breaks the assumption: its postpaid
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
	// Anonymous-safe: the product catalogue is public marketing data — the
	// marketing site SSRs it without a gateway-injected identity.
	if _, ok := accountIDOptional(w, r); !ok {
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
	if _, ok := accountIDOptional(w, r); !ok {
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

// handleRegions returns the sellable region/zone inventory. The console's
// region pickers (top bar + every BuyWizard) render this — the client never
// hardcodes a region list, so opening a new region is a catalogue row, not a
// frontend release.
func (s *catalogStore) handleRegions(w http.ResponseWriter, r *http.Request) {
	if _, ok := accountIDOptional(w, r); !ok {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0, len(s.regions))
	for _, rg := range s.regions {
		zones := make([]map[string]any, 0, len(rg.Zones))
		for _, z := range rg.Zones {
			zones = append(zones, map[string]any{"zoneId": z.ZoneID, "zoneName": z.ZoneName})
		}
		out = append(out, map[string]any{
			"regionId": rg.RegionID, "regionName": rg.RegionName, "zones": zones,
		})
	}
	writeJSON(w, "OK", out)
}

// --- M-8 multi-region topology (09-roadmap §5.2 M-8, 00§4.2/§4.5) --------------

// phase3Plan is the 两地三中心 topology: one in-city dual-active primary
// (cn-north-1, the P2 dual-AZ shape) plus one remote standby (cn-east-1,
// >300km, read-only/DR — Role.Writable() is false, P3 does not promise
// 异地多活写). The classification math lives in pkg-go/multiregion; this seed
// is the plan the cross-region DR drill (tools/cross-region-failover-drill.md)
// executes, matching the GitOps env split (primary carries dev/staging/prod,
// standby carries prod only, 08§4.2).
var phase3Plan = multiregion.Plan{
	Primary: multiregion.Region{Name: "cn-north-1", Role: multiregion.RolePrimary, ZoneLetters: []string{"a", "b"}},
	Standby: multiregion.Region{Name: "cn-east-1", Role: multiregion.RoleStandby, DistanceKm: 1100, ZoneLetters: []string{"a"}},
	Channels: []multiregion.ReplicationChannel{
		multiregion.CoreLedgerChannel(),
		multiregion.ObjectStorageChannel(),
	},
}

// replicationLag is the in-memory stand-in for the region_replication_status
// rows (orchestrator sql/V3): the latest observed lag per cross-region channel.
// Production is written by the replication monitor; the RPO verdict below is
// still computed by the pkg (MeetsRPO), never hand-labelled.
var replicationLag = map[string]time.Duration{
	"ledger-binlog":  2100 * time.Millisecond, // vs 5s RPO
	"object-storage": 8 * time.Minute,         // vs 1h RPO
}

// phase3Services is the core commercial-loop slice classified for the console
// view. The authority is multiregion.Classify — unknown names error loudly, so
// this list can only reference services the pkg knows (IAM/计费 are the two
// GLOBAL singletons, 09§5.2 M-8).
var phase3Services = []string{
	"svc-iam", "svc-billing", "svc-order", "svc-payment", "svc-metering",
	"svc-orchestrator", "svc-quota", "svc-catalog", "svc-monitor",
}

// phase3States is the cross-region state taxonomy the pkg declares
// (ClassifyState's four legal names: account/ledger/object-storage/kafka-topic).
var phase3States = []string{"account", "ledger", "object-storage", "kafka-topic"}

func regionTopologyView(r multiregion.Region) map[string]any {
	zones := make([]string, 0, len(r.ZoneLetters))
	for _, z := range r.ZoneLetters {
		zones = append(zones, r.Name+"-"+z)
	}
	return map[string]any{
		"name": r.Name, "role": string(r.Role), "writable": r.Role.Writable(),
		"distanceKm": r.DistanceKm, "zones": zones,
	}
}

// handleRegionTopology returns the M-8 topology for the console's 地域与容灾
// view: region roles (who writes), the replication channels with live RPO
// verdicts, the canonical 灾备接管 sequence, and the global/regional
// classification. Everything computed comes from pkg-go/multiregion — this
// handler only assembles, it never re-derives.
func (s *catalogStore) handleRegionTopology(w http.ResponseWriter, r *http.Request) {
	if _, ok := accountIDOptional(w, r); !ok {
		return
	}
	if err := phase3Plan.Validate(); err != nil {
		writeErr(w, "Common.InternalError", 500, "region topology plan invalid: "+err.Error())
		return
	}

	channels := make([]map[string]any, 0, len(phase3Plan.Channels))
	for _, c := range phase3Plan.Channels {
		lag := replicationLag[c.Name]
		channels = append(channels, map[string]any{
			"name": c.Name, "mode": c.Mode,
			"rpoMs": c.RPO.Milliseconds(), "lagMs": lag.Milliseconds(),
			"rpoMet": c.MeetsRPO(lag),
		})
	}

	steps := make([]map[string]any, 0, 2)
	for _, st := range phase3Plan.FailoverSteps() {
		steps = append(steps, map[string]any{"name": st.Name, "detail": st.Detail})
	}

	scopes := make([]map[string]any, 0, len(phase3Services))
	for _, svc := range phase3Services {
		scope, err := multiregion.Classify(svc)
		if err != nil {
			writeErr(w, "Common.InternalError", 500, err.Error())
			return
		}
		scopes = append(scopes, map[string]any{"service": svc, "scope": string(scope)})
	}

	states := make([]map[string]any, 0, len(phase3States))
	for _, st := range phase3States {
		class, err := multiregion.ClassifyState(st)
		if err != nil {
			writeErr(w, "Common.InternalError", 500, err.Error())
			return
		}
		states = append(states, map[string]any{"state": st, "class": string(class)})
	}

	writeJSON(w, "OK", map[string]any{
		"plan": map[string]any{
			"primary":  regionTopologyView(phase3Plan.Primary),
			"standby":  regionTopologyView(phase3Plan.Standby),
			"maxRtoMs": multiregion.MaxRTO.Milliseconds(),
		},
		"channels":      channels,
		"failoverSteps": steps,
		"serviceScopes": scopes,
		"stateClasses":  states,
	})
}

// handleImages returns the public image inventory, optionally filtered by
// productCode. The purchase wizard renders it as the 镜像 dropdown so a boot
// request can only carry an image provisioning actually knows.
func (s *catalogStore) handleImages(w http.ResponseWriter, r *http.Request) {
	if _, ok := accountIDOptional(w, r); !ok {
		return
	}
	productCode := r.URL.Query().Get("productCode")
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0)
	for _, im := range s.images {
		if productCode != "" && im.ProductCode != productCode {
			continue
		}
		out = append(out, map[string]any{
			"imageId": im.ImageID, "name": im.Name, "os": im.OS,
			"arch": im.Arch, "productCode": im.ProductCode, "status": im.Status,
		})
	}
	writeJSON(w, "OK", out)
}

// handleCategories returns the marketing taxonomy. The site's category grid
// and the console's product nav render it; product rows join on product.Category.
func (s *catalogStore) handleCategories(w http.ResponseWriter, r *http.Request) {
	if _, ok := accountIDOptional(w, r); !ok {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]category, 0, len(s.categories))
	out = append(out, s.categories...)
	writeJSON(w, "OK", out)
}

// handlePlacement returns the placement contract for a product: its scope
// (REGIONAL/ZONAL), whether it carries cross-AZ replicas, and whether a zoneId
// is required at create time. The console uses this to render the AZ picker in
// the purchase wizard (a ZONAL product shows the zone dropdown, a REGIONAL one
// does not), and the quote path enforces the same contract server-side.
// (M-6, 00§4.4.)
func (s *catalogStore) handlePlacement(w http.ResponseWriter, r *http.Request) {
	if _, ok := accountIDOptional(w, r); !ok {
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
// granularity: EUECI postpaid is per-SECOND (M-7.1, 09 §4.2), while every other
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
		writeErr(w, "Common.MissingAccountId", 403, "X-Euler-Account-Id header is required")
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed X-Euler-Account-Id")
		return 0, false
	}
	return id, true
}

// accountIDOptional is accountIDFrom for the public catalogue reads (products/
// skus/placement/regions/images): an absent header is fine (anonymous browse /
// marketing-site SSR), but a present-but-malformed one is still rejected — a
// gateway that forwards garbage is a bug worth surfacing, not swallowing.
func accountIDOptional(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.Header.Get(accountIDHeader)
	if raw == "" {
		return 0, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeErr(w, "Common.InvalidParameter", 400, "malformed X-Euler-Account-Id")
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, code string, data any) {
	rid := w.Header().Get("X-Euler-TraceId")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Data": data})
}

func writeErr(w http.ResponseWriter, code string, status int, msg string) {
	rid := w.Header().Get("X-Euler-TraceId")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"RequestId": rid, "Code": code, "Message": msg})
}

func requestIDMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Euler-TraceId")
		if id == "" {
			id = fmt.Sprintf("catalog-%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Euler-TraceId", id)
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

	repo, err := newCatalogRepo(context.Background())
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	loadCtx, loadCancel := context.WithTimeout(context.Background(), loadDeadline)
	store, err := newCatalogStoreFrom(loadCtx, repo)
	loadCancel()
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	// Log where the catalogue lives: prices and placements edited by ops in the
	// database take effect on restart, and a store still running on its seed is
	// a very different animal.
	slog.Info("svc-catalog catalogue ready", "persistent", persistentCatalogRepo(repo),
		"products", len(store.products), "skus", len(store.skus), "rules", len(store.rules))
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ready")) })
	mux.HandleFunc("POST /api/v1/catalog/quote", store.handleQuote)
	mux.HandleFunc("GET /api/v1/catalog/products", store.handleProducts)
	mux.HandleFunc("GET /api/v1/catalog/skus", store.handleSKUs)
	mux.HandleFunc("GET /api/v1/catalog/placement", store.handlePlacement)
	mux.HandleFunc("GET /api/v1/catalog/regions", store.handleRegions)
	mux.HandleFunc("GET /api/v1/catalog/region-topology", store.handleRegionTopology)
mux.HandleFunc("GET /api/v1/catalog/images", store.handleImages)
mux.HandleFunc("GET /api/v1/catalog/categories", store.handleCategories)

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
