# StarCloud Platform — Implementation

This repository implements the self-built cloud platform specified in
`docs/architecture/`. The architecture docs are the binding source of truth;
code is built against them chapter by chapter.

## Phase-1 scope (09-roadmap §3.1)

9 months to deliver 7 sellable products (SCVPC/SCECS/SCBS/SCOSS/SCRDS/
SCMON/SCEIP) and 17 microservices, closing the commercial loop:
注册 → 实名 → 充值/下单 → 开通 → 计量 → 出账 → 欠费治理 → 释放.

**Status: the phase-1 domain logic is complete and tested.** Every link of the
commercial loop exists as a tested Go package with a runnable demo. In addition,
every phase-1 service now has a runnable stdlib HTTP server, a proto contract in
`proto-hub`, a DDL migration, and a Helm chart; the console/portal frontend
scaffold is in `platform/frontend`. What remains is infrastructure execution —
see [Verification status](#verification-status).

## Phase-2 progress (09-roadmap §4, milestones M-4 → M-5 → M-6 → M-7)

The four billing forms (M-4) are live, extending the phase-1 commercial loop;
the OpenAPI ecosystem (M-5) is live, extending the developer surface; the
dual-AZ topology (M-6) is live, turning the reserved region/AZ model into an
implemented P2 failure-domain contract; and the product matrix (M-7) is
complete — eight new products (SCECI/SCLB/SCAS/SCBACKUP/SCREDIS/SCKAFKA/SCLOG) + RAM sub-accounts
onboarded via the 06§4.1 four-component pattern, each passing the MockDriver
验收门槛 (06§6.3).

### M-4 — billing four forms

Three new `pkg-go` packages and svc-billing/console-bff wiring:

- **资源包抵扣** (M-4.1) — `pkg-go/reservepack`: a prepaid-quota ledger consumed
  at rank 0 of the billing waterfall (`billing.SourceResourcePack`, unchanged
  waterfall). Idempotent consume/refund/expire, optimistic lock, expiry sweep.
- **抢占式浮动价 + 回收** (M-4.2) — `pkg-go/spot`: an append-only floating-price
  engine (every cycle billed against the price snapshot at cycle start).
  `provision.Preemptor` (optional Driver capability) is the two-step reclaim
  handshake: notify → 5-min window → reclaim, gated by delivered notice.
  `resource.StatePreempting` extends the lifecycle machine.
- **发票 / 红冲 / 成本分析** (M-4.3) — `pkg-go/invoice`: 红冲 issues a full-negative
  reversal, original → VOIDED terminal (retained, like the audit chain).
  `billing.CostReport` breaks spend down by product/resource, agreeing with
  `Summarize` (无未解释差异).
- **前端** (M-4.4) — `web-billing` gains ResourcePackages / Invoices / CostAnalysis
  views; `console-bff` proxies them to svc-billing with the same fan-out + 503
  contract.

`pricing.Sellable()` opens all four billing forms (包年包月 / 按量 / 资源包 /
抢占式); `SellableInPhase1()` is retained as the phase-1 historical record. The
phase-1 gate semantics are unchanged.

### M-5 — OpenAPI ecosystem (Explorer + Go/Python SDK + versioning)

One contract, three consumers (03§9.4 rule ⑤): the SDK, the gateway verifier,
and the OpenAPI Explorer share `pkg-go/cps1` — no parallel signer exists, and
drift is caught by the golden-vector fixture, not by customers.

- **OpenAPI Explorer** (M-5.1) — svc-api-meta `/api/v1/apimeta/explorer` signs a
  product API call with cps1 and optionally proxies it to a configured target;
  `cps1.SignedHeaderList` is the shared export the Explorer renders (not a fork).
  New `devops-explorer` Wujie sub-app: API catalogue + parameter form + live
  signature/response view. Explorer smoke-tested over real HTTP (missing-account
  403, signature matches the golden-vector body hash).
- **Go + Python SDK** (M-5.2) — `pkg-go/scsdk` (Go) and `sdk/python/cloudsdk`
  (Python) sign with cps1 and parse responses into `pkg-go/errors`. The Python
  signer reproduces all 7 golden-vector signatures byte-for-byte (cross-language
  regression, the fixture `proto-hub/testdata/cps1-golden-vectors.json`).
- **Versioning & deprecation** (M-5.3) — `proto-hub/VERSIONING.md` codifies the
  additive-only-within-a-major-version policy; `tools/check-proto-breaking.py`
  is the source-only CI guard (field-number removal/retype/reuse, proto3
  `required`, unversioned packages), wired into `manifest-repo-pipeline.yml`.
  Negative-tested: removing a field is caught; baseline pins 699 fields.

### M-6 — dual-AZ (同城双 AZ, P2)

The phase-1 reserved region/AZ model is now implemented as a P2 dual-AZ
topology (00§4.4, 09-roadmap §4.3 M-6, gate B3: AZ-level failover RTO ≤ 5min).

- **`pkg-go/topology`** — Region/AZ model, `RegionScope` (REGIONAL/ZONAL), and
  the cross-AZ spread + failover-survival reasoning. The non-obvious invariant
  it encodes: an *even spread across 2 AZs never survives generic AZ loss*
  (the larger zone always holds ≥ half), so MySQL MGR uses its own
  majority rule — and a 3-node MGR group across 2 AZs as (2,1) survives loss of
  the *lighter* AZ only; either-AZ survival needs 3 AZs (P3). `CanSatisfy` is
  the create-time preflight; `PlacementReport` names the at-risk zones a drill
  should target. Region/AZ *names* are not re-defined here — `identifier.AZName`
  stays the single source.
- **svc-catalog RegionScope made meaningful** (M-6.1b) — `scecs`/`scrds`/`scbs`/
  `sceip` are now ZONAL (pinned to one AZ at create time); `scvpc`/`scoss`/
  `scmon` are REGIONAL. The quote path enforces it: a ZONAL product's 询价
  carries a `zoneId` that must be a valid AZ in the quoted region (rejected at
  400 `Catalog.ZoneRequired`/`InvalidZone`/`ZoneRegionMismatch`, before any
  order is created). New `/api/v1/catalog/placement` exposes the contract to
  the console's AZ picker.
- **DDL V2 resource_db** (M-6.1c) — `resource_instance.az_id` formalised (the
  phase-1 `zone` placeholder is now a queryable fault-domain dimension), with
  `idx_region_az` / `idx_az_status` for AZ-level failover scanning. Two views
  (`v_resource_by_az`, `v_resource_az_failover_impact`) let the drill answer
  "if this AZ goes, whose resources break?" from one SQL.
- **Cross-AZ middleware manifests** (M-6.2a) — Kafka KRaft 3-broker (hard
  per-zone anti-affinity, `min.insync.replicas=2`), MySQL MGR single-primary
  (soft anti-affinity — 3 across 2 AZs is the (2,1) the math permits, `maxSkew:2`
  forbids the (3,0) concentration), Redis Cluster 3 shards × master+replica
  (each pair hard-spanning AZs so either-AZ survival holds). The phase-1
  "unwritten" middleware manifests.
- **Helm dual-AZ topology** (M-6.2b) — deployment templates gained a zone-level
  `topologySpreadConstraints` (maxSkew:1, DoNotSchedule, hard) gated by a
  `topology.zoneSpread` value; prod/staging turn it on, dev stays single-AZ.
- **Topology validator** (M-6.2c) — `tools/check-topology-spread.py` is the
  source-only CI guard: a stateful workload (Kafka/MySQL/Redis) with no zone
  anti-affinity/spread is caught (the failure is silent in production until an
  AZ actually fails), and a `zoneSpread` override whose template has no code
  path is caught as a placebo. Negative-tested against stripped Redis shards.
  Wired into `manifest-repo-pipeline.yml` as `validate:topology-spread`.
- **AZ failover drill runbook** (M-6.3) — `tools/az-failover-drill.md`: the
  quarterly AZ-level drill (drain AZ → verify MGR primary elect, Kafka ISR,
  Redis failover → RTO≤5min). Documents the §5 boundary: the drill targets the
  *lighter* AZ, because 2-AZ MGR (2,1) cannot survive losing the heavier side.

### M-7 — product matrix GA (09-roadmap §4.3 M-7)

Every new product follows the 06§4.1 four-component pattern (catalogue
registration → CRD → Operator → metering point) and must pass the MockDriver
full-chain 验收门槛 (06§6.3) before launch. Six products + RAM landed, each
verified over real HTTP end-to-end.

- **SCECI 弹性容器实例** (M-7.1, P0) — the elastic container instance: seconds-scale
  bring-up, per-second POSTPAID billing (09 §4.2, the `DurationSecond` unit M-4.2
  reserved), the "弹性" partner to SCECS's VM 旗舰 (09 §3.2 D-03). Four components:
  - **Catalogue** — `svc-catalog` registers `sceci` (ZONAL, `resource_type=eci`),
    three postpaid-only SKUs, and per-SECOND pricing rules (rule IDs 18-20).
    The quote path derives the `DurationUnit` from the matched rule (not a
    hardcoded assumption), so a new billing granularity is a catalogue row,
    not a code change — `pricing.PricingRule.Active`/`Specificity` exported for
    this single-source reuse.
  - **CRD** — `services/rc-eci/crd/sceci-instance-crd.yaml` (`SceciInstance`):
    the desired-state model, per-second `cpu_core_second`/`mem_gb_second`
    metering as a first-class `status.usage` field, K8s-native conditions
    (PodScheduled/ContainerReady), the four mandatory labels + finalizer.
  - **Operator** — `services/rc-eci` `go run ./cmd/rc-demo` is the 06§6.3
    MockDriver 验收门槛: 22 checks covering dispatch → reconcile →
    evidence-not-authority adjudication (S21) → idempotency → arrears suspend
    (config retained, billing stops) → `DriverK8s` binding (sceci→K8s,
    scecs→VM — distinct backends by catalogue config) → compensation-failure
    ticket. All pass.
  - **Frontend** — `console-eci` sub-app (Wujie, port :5183): InstanceList
    (filtered to sceci), BuyWizard (postpaid-only, AZ picker, per-second quote
    → 预估每小时), InstanceDetail. Registered in the console-base shell menu.

- **SCLB 负载均衡** (M-7.2, P0) — 4/7-layer load balancer (APISIX L7 + LVS/IPVS L4,
  09 §4.2). REGIONAL (an LB spans AZs — it is the cross-AZ entry point),
  postpaid by usage (LCU + traffic). `services/rc-lb` CRD `SlbInstance` +
  rc-demo (06§6.3 gate, sceci-style), `console-lb` sub-app (:5184, REGIONAL
  wizard — no zone picker, listener/backend config).

- **SCAS 弹性伸缩** (M-7.3, P2) — the scaling policy layer over HPA/VPA/CA
  (06 §2.6). Ships a real domain engine `pkg-go/autoscaling`: `Evaluate`
  decides scale-up/down from metrics, deny-first (no-op unless a rule
  matches), cooldown anti-flapping, min/max clamping, first-match-wins. The
  executor (HPA/VPA/CA) is separate — this package decides WHAT to do.
  REGIONAL, postpaid management fee. `services/rc-autoscaling` CRD
  `ScalingGroup` + rc-demo (incl. a policy-engine scenario), `console-autoscaling`
  (:5185, REGIONAL, min/max/desired + cpu-threshold rule).

- **SCBACKUP 云备份** (M-7.4, P2) — scheduled snapshot + cross-AZ backup policy.
  Ships `pkg-go/backup`: `NextRun` (first-run=now), `IsDue`, `ExpiredSnapshots`
  (retention enforced, 0=forever), `Validate`. Retention is never unbounded —
  the policy decides WHAT to expire, the resource reconcile loop does it.
  REGIONAL, postpaid by stored capacity. `services/rc-backup` CRD
  `BackupPolicy` + rc-demo (incl. policy-semantics scenario),
  `console-backup` (:5186, schedule + retention + crossAz toggle).

- **SCREDIS 托管 Redis** (M-7.5, P1) — managed Redis as a productized middleware
  (09 §4.2: the platform's own redis-cluster ops experience, M-6.2a, turned into
  a product). ZONAL with cross-AZ HA (master+replica across AZs). Both prepay
  and postpay (managed DB convention, like scrds). `services/rc-redis` CRD
  `ScredisInstance` + rc-demo, `console-redis` (:5187, ZONAL wizard with HA
  toggle, prepay/postpay).

- **SCKAFKA 托管 Kafka** (M-7.5, P1) — managed Kafka as a productized middleware
  (09 §4.2: the platform's own kafka-kraft ops experience, M-6.2a, turned into a
  product). ZONAL with cross-AZ HA (brokers across AZs, min.insync.replicas=2
  tolerates one AZ loss). Both prepay and postpay (managed middleware
  convention, like scredis). `services/rc-kafka` CRD `SckafkaInstance` +
  rc-demo (24 checks), `console-kafka` (:5188, ZONAL wizard with AZ picker,
  brokerCount/partitionCount/retentionHours + crossAz HA toggle, prepay/postpay).

- **SCLOG 日志服务** (M-7.5, P1) — Vector collect + ClickHouse store productized
  (09 §4.2, multi-tenant topic/table). REGIONAL ingestion + storage; cross-AZ
  storage is a replica flag, not a placement constraint. Postpaid by
  storage-hour + ingestion-by-volume. `services/rc-logservice` CRD
  `SclogInstance` + rc-demo, `console-logservice` (:5189, REGIONAL wizard, no
  zone picker, retentionDays/storageGb + crossAz toggle). Retention floor is
  ≥1 day (not 0=forever like backup) — unbounded log growth is a disk-full
  hazard.

- **RAM 子账号体系** (M-7.6, 07 §10 M1) — extends the phase-1 `pkg-go/authz`
  Deny-first engine with a **policy simulator** (`pkg-go/authz/simulator.go`):
  `Simulate` evaluates a request against a policy set, naming the deciding
  statement, Deny-first (explicit Deny wins; absent any match → default_deny).
  svc-iam `web-auth` gains `/api/ram/{roles,policies,simulate}` (5 endpoints,
  Bearer-auth); `web-account` gains RoleList + PolicySimulator views. The
  simulator lets an operator test "would principal X be allowed?" BEFORE
  committing a policy — a mis-scoped policy caught at design time, not at the
  first customer-data incident.

**M-7 verification** — every rc-* demo passes the 06§6.3 MockDriver gate (8
products: SCECI/SCLB/SCAS/SCBACKUP/SCREDIS/SCKAFKA/SCLOG + RAM);
`pkg-go` 26 packages green (autoscaling + backup added); `svc-catalog` 21
tests (incl. placement/quote tests for the new products — REGIONAL
sclb/scas/scbackup/sclog ignore zone, ZONAL sceci/scredis/sckafka enforce
zoneId + accept prepaid where offered);
`svc-iam web-auth` 7 RAM tests + authz 21 tests; frontend `pnpm -r typecheck`
26 projects pass; 5 repo validators pass (164 yaml incl. 10 new CRDs);
real-HTTP e2e for all products (placement → quote → order → pay → fulfill →
RUNNING, 7 products coexisting) + RAM simulator (allow/default_deny/
explicit_deny-wins/404/400).

**Phase-2 architecture-doc writeback** — `docs/architecture/09-roadmap.md`
gains §4.0 "二期实装状态回写" (milestone table + 8-product inventory +
B1–B7 gate verification + items deferred to phase-3); `10` and `11` each add a
one-line implementation traceability pointer back to 09 §4.0 (without
disturbing their forward-looking spec). This closes the gap where the code was
shipped but the spec still read M-4–M-7 as future plan.

## Phase-3 progress (09-roadmap §5, milestones M-8 → M-9 → M-10 → M-11)

Phase 3 is 规模化与可信度 (T0+18~T0+30): multi-region, stability engineering
(SLO/chaos), ecosystem (marketplace/Terraform), and compliance (等保三级).
The first milestone, M-8 (第二地域点亮, 09§5.2), turns the reserved region
field into an implemented P3 contract.

### M-8 — second region lights up (multi-region model)

- **`pkg-go/multiregion`** — the cross-region layer above `pkg-go/topology`
  (which stays the AZ-level P2 layer). It encodes 00§4.1/§4.5 and 09§5.2 M-8:
  - `Region{Name, Role, DistanceKm, ZoneLetters}` with PRIMARY (in-city
    dual-active) vs STANDBY (remote >300km, read-only/DR) roles;
    `Role.Writable()` encodes the P3 boundary "不承诺异地多活写" — only
    PRIMARY writes.
  - `Classify(service)` — the global-vs-regional classification: IAM and
    billing are GLOBAL singletons (09§5.2 M-8 "IAM/计费全局单例"), everything
    else REGIONAL; unknown service names are rejected, not defaulted.
  - `ClassifyState` — account = SHARED (the only global domain, 00§4.1),
    ledger/object-storage = REPLICATED, Kafka topics = REBUILT (00§4.5 "不做
    跨城镜像, 异地按 cloud.* 规范重建 topic").
  - `Plan.Validate` — exactly one PRIMARY, standby >300km, non-negative RPOs.
  - `ReplicationChannel.MeetsRPO` + `MeetsRTO` (MaxRTO = 30min) + the
    canonical `FailoverSteps` (管控面冷转热 + DNS 切换).
  - Region names delegate to `identifier.IsValidRegion` (single source, not
    forked — the M-6 lesson carried up one layer).
- **`pkg-go/event`** — `SysReplicationStatus` (`cloud.sys.replication.status`)
  topic for cross-region replication-lag observability; the comment records
  that Kafka topics themselves are rebuilt per region, not mirrored.
- **Multi-region IaC** — `deploy/gitops-manifests/envs/` restructured to the
  08§4.2 shape `envs/<region>/<env>/values-overrides/` (primary cn-north-1
  carries dev/staging/prod; standby cn-east-1 carries prod only). The
  ApplicationSet gains a region dimension (matrix charts × region × env), and
  `platform/middleware/minio-replication.yaml` adds the async object-storage
  replication channel. `tools/check-topology-spread.py` reads the new path.
- **DDL V3** — `services/svc-orchestrator/sql/V3__resource_db_region_topology.sql`:
  `region_replication_status` (cross-region replication watermark, the RPO
  observability source) + `v_resource_by_region` /
  `v_resource_region_failover_impact` (region-level failover impact for the
  M-11 drill). Shard key unchanged (account_id single key; region stays a
  metadata dimension).

### M-9 — stability platform (SLO + chaos + release gates)

- **`pkg-go/slo`** — error budget = 1 − SLO; multi-window burn-rate alerting
  (1h at ≥14.4× pages, 3d at ≥1× tickets); budget policy (<50% remaining slows
  releases, exhausted freezes them); the SLA gate requires two consecutive
  quarters of SLO compliance (08§10). The platform SLO seed carries the 08§10.2
  targets (gateway 99.95%, metering 99.99%, billing-on-time 99.5%, ...).
- **`pkg-go/chaos`** — the six mandatory drill subjects (08§9.5), the prod
  discipline (named blast radius + abort ≤10min), and the >50%
  expected-vs-actual deviation that opens a remediation item.
- **`tools/chaos-drill-runbook.md`** — the quarterly drill carrier: the six
  subjects, staging-first, the prod abort path, and the archive/adjudication
  rules, all aligned with `pkg-go/chaos`.
- **`pkg-go/release`** — the 变更三板斧 as code: canary 5→20→50→100 with an
  analysis gate (success ≥0.995, p99 ≤1.5s), expand-contract observation ≥7
  days, and git-revert-first rollback (08§6).

### M-10 — ecosystem (marketplace + Terraform)

- **`pkg-go/settlement`** — the marketplace revenue split; partner + platform
  shares always sum back to the gross EXACTLY (no unexplained difference).
- **`services/svc-marketplace`** — the marketplace closed loop (上架审核 →
  分账/结算) over real HTTP: publish → PENDING_APPROVAL, approve → APPROVED,
  settle → an immutable split (idempotent per order). `proto-hub` gained
  `marketplace/v1`; DDL + Helm chart + env overrides ship with it.
- **`sdk/terraform`** — the Terraform Provider skeleton (source-only): four core
  products (SCECS/SCOSS/SCVPC/SCRDS) sharing one `crudCall` helper that signs
  via `pkg-go/scsdk`/`cps1` — no parallel signer.
- **开发者社区 (M-10.3)** — `devops-explorer` gains a `/community` view: docs
  entry points (Go/Python SDK, Terraform, VERSIONING), signed-call examples for
  all three, and the community entry (GitHub repo); real signing stays in
  `pkg-go/cps1`, the view only mirrors SDK entry points.

### M-11 — phase-3 GA artifacts

- **`tools/cross-region-failover-drill.md`** — the two-region DR drill (异地冷
  转热 + DNS 切换, RPO≤5min/RTO≤30min), layered above the AZ-level
  `az-failover-drill.md`.
- **`tools/dengbao-level3-checklist.md`** — the 等保三级 checklist (07§6.1) +
  the audit-retention 口径 (180d hot + MinIO cold; 365d/18-month paid tier).

### Deferred phase-2 items landed in phase 3

- **云监控高级告警 (alert-center)** — `pkg-go/alertcenter` (dedup/group/inhibit/
  silence + 10/min tenant rate limit) + `services/alert-center` (:9213).
- **异常用量检测** — `pkg-go/anomaly` (z-score over a rolling baseline, flat
  baseline handled without ±Inf) + `svc-metering /api/v1/metering/anomaly-scan`.
- **STS 临时凭证** — `pkg-go/sts` (expiring AK/SK/token; positive TTL enforced)
  + `svc-iam /api/sts/assume-role`.

**Phase-3 architecture-doc writeback** — `docs/architecture/09-roadmap.md` gains
§5.0 "三期实装状态回写" (milestone table M-8→M-11 + C1–C5 gate verification +
deferred-item closure), mirroring the §4.0 phase-2 writeback.

## Repository layout

```
docs/architecture/        Spec (binding source of truth; chapter 00-11)
docs/cps1-implementation-notes.md   CPS1 contract notes (encoding, verify order)
docs/apisix-gateway-notes.md        Gateway chain, exemptions, fail-closed rules
proto-hub/                IDL single source of truth (buf lint + breaking)
  proto/starcloud/{iam,event,catalog,order,payment,billing,metering,
    orchestrator,quota,workflow,monitor,notify,audit,ticket,org,alert,apimeta}/v1/*.proto
  testdata/cps1-golden-vectors.json  Cross-language signature fixture
  VERSIONING.md            Additive-only / deprecation policy (M-5.3)
  .breaking-baseline.json  Field-number baseline for the breaking-change gate

pkg-go/                   Shared domain libraries (33 packages, 505 top-level tests)
  — identity & access —
  cps1/                   CPS1-HMAC-SHA256 signer/verifier (07§4.1)
  scsdk/                  Go client SDK: signs via cps1, errors via errorsx (M-5.2)
  kms/                    Envelope encryption, key hierarchy (07§5.3)
  accesskey/              AK/SK lifecycle: max 2, rotation grace (07§2.2)
  authz/                  RAM policy engine, Deny-first + policy simulator (07§3, M-7.6)
  verify/                 Gateway forward-auth chain (07§4.1)
  sts/                    Temporary credentials (AssumeRole → AK/SK/token, expiring) — phase 3
  — commerce —
  pricing/                Fixed-point money + pricing engine (01§7, §12.3)
  order/                  Unified order model + state machine (03§4.2.2)
  ledger/                 Cash balance + append-only journal (S29, 03§8.5)
  settlement/             Marketplace revenue split (partner+platform≡gross) — M-10
  — resources —
  resource/               Resource lifecycle + arrears/expiry (03§5.2, 01 D8)
  workflow/               Saga engine with compensation (03§4.3.3, §8.4)
  provision/              ProvisionDriver + CRD conventions (06§4, §6.2)
  quota/                  Two-phase reservation with TTL (03§4.3.2)
  — billing —
  metering/               Hourly aggregation + covered_ratio (05§5)
  billing/                Deduction waterfall + reconciliation (03§4.2.4)
  reservepack/            Resource-pack quota ledger — phase 2 M-4.1 (D6)
  spot/                   Spot floating-price engine — phase 2 M-4.2 (D6)
  invoice/                Invoice + 红冲 reversal — phase 2 M-4.3 (B6)
  autoscaling/            Scaling policy engine (deny-first, cooldown) — M-7.3
  backup/                 Backup schedule + retention — M-7.4
  anomaly/                Usage anomaly detection (z-score over rolling baseline) — phase 3
  — support —
  notify/                 Delivery evidence for trust-critical classes (03§4.4.2)
  audit/                  Tamper-evident hash chain (07§6)
  alertcenter/            Alert convergence (dedup/group/inhibit/silence) + tenant rate limit — phase 3
  — platform —
  identifier/             Global identifier conventions (00 附录A)
  topology/               Region/AZ model + cross-AZ spread + failover math (M-6)
  multiregion/            Cross-region model + global/regional classification (M-8)
  event/                  Kafka envelope + topic constants (04§5.4)
  errors/                 Unified OpenAPI error model (03§9.3)
  — stability (phase 3) —
  slo/                    Error budget, burn-rate alerting, budget policy (M-9)
  chaos/                  Drill plans, mandatory subjects, >50% deviation remediation (M-9)
  release/                Canary gates, expand-contract window, rollback preference (M-9)

services/                 每个一期服务均有 cmd/server 可运行 HTTP 服务
  _tmpl-go/               Go Kratos archetype (OTel/Nacos/health/idempotent)
  svc-iam/                account/RAM/AK-SK/KMS/policy + runnable verify endpoint
  svc-org/                组织/项目(资源组)/标签 (03§4.1.2)
  svc-catalog/            商品化中台: product registry, SKU, pricing rules
  svc-order/              订单中心: five transaction types, one model
  svc-payment/            资金侧: balance, ledger, payment/refund
  svc-billing/            出账/抵扣/余额/欠费判定 (03§4.2.4)
  svc-metering/           计量: aggregation, backfill, reconciliation
  svc-orchestrator/       资源编排: lifecycle ledger, saga fulfilment
  svc-quota/              配额: two-phase occupy + support-domain demo
  svc-workflow/           轻量工作流/状态机引擎 (03§4.3.3)
  svc-monitor/            监控告警规则管理与查询代理 (03§4.4.1)
  svc-notify/             消息通知 + 信任关键留证 (03§4.4.2)
  svc-audit/              操作审计采集与防篡改链 (03§4.4.3)
  svc-ticket/             工单系统 (03§4.4.4)
  svc-api-meta/           OpenAPI 元数据/文档/SDK 中心 + Explorer 调试端点 (03§4.5.1, M-5.1)
  console-bff/            控制台聚合层 (03§3)
  svc-marketplace/        云市场: 上架审核/分账结算, proto+DDL+chart (M-10, :9212)
  alert-center/           告警收敛/对客通道: 4级收敛+10条/分钟限流 (D-1, :9213)
  rc-compute/             数据面控制器: ProvisionDriver + SCECS CRD
  rc-eci/                 SCECI 弹性容器实例 控制器 + CRD (M-7.1, DriverK8s)
  rc-lb/                  SCLB 负载均衡 控制器 + CRD (M-7.2, APISIX/LVS)
  rc-autoscaling/         SCAS 弹性伸缩 控制器 + CRD (M-7.3)
  rc-backup/              SCBACKUP 云备份 控制器 + CRD (M-7.4)
  rc-redis/               SCREDIS 托管 Redis 控制器 + CRD (M-7.5)
  rc-kafka/               SCKAFKA 托管 Kafka 控制器 + CRD (M-7.5)
  rc-logservice/          SCLOG 日志服务 控制器 + CRD (M-7.5)
  (rc-storage/rc-network/rc-database 的 Helm chart 已就绪,控制器代码复用 rc-compute 模式)

sdk/python/              cloudsdk — Python SDK: signs via cps1, golden-vector regression (M-5.2)

sdk/terraform/           starcloud Terraform Provider — source-only; reuses scsdk/cps1 (M-10.2)

platform/frontend/        Vue+Wujie 官网/控制台骨架 (source-only, 02章)
  apps/site/              营销官网: 7 个一期产品
  apps/console-base/      Wujie 微前端基座 + 15 个子应用注册表
  apps/console-ecs/       示例子应用 (mount/unmount 生命周期)
  apps/console-eci/        弹性容器实例 ECI 子应用 (M-7.1, 按秒计费)
  apps/console-lb/         负载均衡 SLB 子应用 (M-7.2)
  apps/console-autoscaling/ 弹性伸缩 AS 子应用 (M-7.3)
  apps/console-backup/    云备份子应用 (M-7.4)
  apps/console-redis/     云数据库 Redis 子应用 (M-7.5)
  apps/console-kafka/     消息队列 Kafka 子应用 (M-7.5)
  apps/console-logservice/ 日志服务 子应用 (M-7.5)
  apps/web-account/       账号与访问控制 + RAM 策略模拟器 (M-7.6)
  apps/devops-explorer/   OpenAPI Explorer 在线调试台 (M-5.1)
  docs/                   02 章要点摘录

deploy/gitops-manifests/  ArgoCD App-of-Apps; dir-as-env, single trunk (08§4.3)
  apps/<svc>/             per-service Helm chart (env-agnostic templates)
  envs/{dev,staging,prod}/values-overrides/   env differences ONLY here
  platform/apisix/        Gateway: plugins (Lua), routes, config, deployment
  platform/middleware/    Cross-AZ stateful topology: Kafka/MySQL/Redis (M-6.2a)
platform/ci-templates/    Shared GitLab CI pipeline templates
tools/                    Repo validators (YAML, Lua, APISIX routes, proto breaking,
                          topology spread) + az-failover-drill runbook (M-6)
```

## Build & test

```sh
cd pkg-go && go test ./...     # 33 packages (incl. slo/chaos/release/...), 505 top-level tests
cd sdk/python && python -m pytest   # Python SDK: 12 tests, incl. 7 golden-vector sigs
```

The Python SDK's golden-vector regression reads
`proto-hub/testdata/cps1-golden-vectors.json` and asserts every
`expected_signature` is reproduced byte-for-byte — the cross-language check that
the Python signer has not drifted from `pkg-go/cps1` (03§9.4 rule ⑤).

Runnable demos — each walks a chapter of the commercial loop and asserts its
invariants:

```sh
cd services/svc-catalog     && go run ./cmd/pricing-demo    # 询价 vs seed catalogue
cd services/svc-order       && go run ./cmd/order-demo      # 下单→支付→履约→退订
cd services/svc-payment     && go run ./cmd/payment-demo    # 充值→冻结→退款→对账
cd services/svc-orchestrator&& go run ./cmd/lifecycle-demo  # 开通→欠费→保留→释放
cd services/svc-metering    && go run ./cmd/metering-demo   # 采集→聚合→出账→账单
cd services/rc-compute      && go run ./cmd/rc-demo         # 下发→收敛→冻结→回收
cd services/svc-quota       && go run ./cmd/support-demo    # 配额→通知留证→审计链
```

Every phase-1 service also runs as a real HTTP server (stdlib, `_tmpl-go`
scaffold pattern; account taken from the gateway-injected `X-Sc-Account-Id`
header, 403 when missing):

```sh
cd services/svc-billing   && go run ./cmd/server   # /internal/{balance,settle,bills}
cd services/svc-workflow  && go run ./cmd/server   # /internal/flows/*
cd services/svc-notify    && go run ./cmd/server   # /internal/notifications/*
cd services/svc-audit     && go run ./cmd/server   # /internal/audit/*
cd services/svc-org       && go run ./cmd/server   # /internal/{projects,tags}
cd services/svc-ticket    && go run ./cmd/server   # /internal/tickets/*
cd services/svc-api-meta  && go run ./cmd/server   # /internal/actions/*
cd services/console-bff   && go run ./cmd/server   # /console/*
```

Wire-level check of the signature path over real HTTP:

```sh
cd services/svc-iam
SC_DEV_SEED_AK=1 go run ./cmd/verify -addr 127.0.0.1:9101   # prints a dev AK/SK
SC_AK=<ak> SC_SK=<sk> go run ./cmd/e2e-verify               # 7 checks
```

## Repo validators

Run before opening an MR; they also run in CI
(`platform/ci-templates/manifest-repo-pipeline.yml`):

```sh
python tools/check-yaml-syntax.py      # every manifest parses (Helm-aware)
python tools/check-lua-plugins.py      # APISIX plugin module contract
python tools/check-apisix-routes.py    # auth coverage, chain order, Nacos form
python tools/check-proto-breaking.py   # proto wire-breaking changes (M-5.3, R14)
python tools/check-topology-spread.py  # cross-AZ placement intent on stateful workloads (M-6.2c)
```

The route validator catches failures that are silent in production: a route
missing `sc-auth` is an unauthenticated endpoint; a Nacos subscription string in
the wrong form resolves to zero upstreams and 502s with nothing in the logs. It
was negative-tested against eight deliberately broken routes and caught all of
them. The proto validator was negative-tested against field removal and
field-number reuse — both wire-breaking — and catches both. The topology
validator was negative-tested against stripped zone constraints (a Redis shard
with no zone anti-affinity) and catches the regression; a stateful workload
"surviving AZ loss" only by claim, not by manifest, is the failure it stops
before an AZ actually fails.

## Single-source-of-truth cross-references

- **Signature algorithm**: `pkg-go/cps1` (07§4.1, D4) — Go SDK (`pkg-go/scsdk`),
  Python SDK (`sdk/python/cloudsdk`), gateway verifier, and OpenAPI Explorer all
  share one impl, pinned by `proto-hub/testdata/cps1-golden-vectors.json`. The
  Python SDK reproduces all 7 golden-vector signatures byte-for-byte. See
  `docs/cps1-implementation-notes.md`.
- **Error model**: `pkg-go/errors` (03§9.3) — both SDKs parse responses into the
  same `{Product}.{Module}.{Reason}` code + HTTP status pairing the wire carries.
- **Global identifiers**: `pkg-go/identifier` (00 附录A).
- **Kafka topics**: `pkg-go/event` (04§5.4) — declared once, referenced by symbol.
- **Error codes**: `pkg-go/errors` (03§9.3).
- **RAM policy semantics**: `pkg-go/authz` (07§3.3) — Deny-first, fail-closed on
  unknown condition operators.
- **API versioning policy**: `proto-hub/VERSIONING.md` (M-5.3) — additive-only
  within a major version; breaking changes need committee sign-off + a bump.
- **Region/AZ topology**: `pkg-go/topology` (M-6, 00§4.4) — Region/AZ model,
  RegionScope (REGIONAL/ZONAL), and the cross-AZ spread + failover math
  (`SurvivesAZLoss` / `SurvivesAZLossMGR`). Names delegate to
  `pkg-go/identifier` (AZName/IsValidRegion), which stays the single source.
- **Pricing-rule selection**: `pricing.PricingRule.Active`/`Specificity` (M-7.1)
  — the rule-matching logic the engine uses internally, exported so a service
  (svc-catalog deriving a quote's `DurationUnit` from the matched rule rather
  than hardcoding it) reuses one implementation.

## Invariants worth knowing before changing code

These are enforced by tests; changing them changes platform behaviour customers
depend on.

| Invariant | Where | Why |
|---|---|---|
| Money is fixed-point, never float | `pricing.Amount` | Float drift across millions of hourly rows makes bills unreconcilable |
| Proration uses `MulDiv`, not basis points | `pricing.Amount.MulDiv` | 11/12 → 9166bp under-refunds; the bias is directional and accumulates |
| `paid → fulfilling` is the only provisioning trigger | `order` | Nothing but a paid order may create a resource |
| Refunds run after resource release | `order.StartRefund` | Otherwise the customer holds both money and a running resource |
| Balance never goes negative | `ledger` | Overdraft is an arrears state, not unauthorised credit |
| Billing starts at RUNNING, never resets | `resource` | Fair to the customer; stop/start cannot reset the clock |
| Release requires a delivered warning | `resource.Release` + `notify` | Data is never destroyed without provable notice |
| Trust-critical notifications bypass throttling | `notify` | Throttling an arrears warning defeats its purpose |
| Quota counts in-flight reservations | `quota` | Ignoring them lets two orders take the last slot |
| Compensation runs in reverse order | `workflow` | Quota must be released after the resource is deleted |
| Audit chain is tamper-evident | `audit` | Permissions can be changed by whoever holds them |
| Unknown authz condition → deny | `authz` | Fail closed on anything not understood |
| `Amount` is micro-units (1/1e6 yuan), not yuan | `pricing.Amount` | `Amount(300)` is 0.0003 yuan — a silent scale trap; use `MustParseAmount` |
| Resource-pack consume is idempotent + optimistic-locked | `reservepack` | A retried settlement cannot double-spend quota |
| Spot price history is append-only | `spot` | A bill issued today for last week's cycles must still reconcile |
| 红冲 is a reversal, not a delete | `invoice` | Tax law forbids deleting an issued invoice; original → VOIDED terminal |
| Reclaim requires delivered 5-min notice | `provision.Preemptor` | The platform cannot reclaim what it has not warned about |
| One signing implementation, three consumers | `cps1` | SDK + Explorer + gateway share one signer; the Python SDK must reproduce the golden vectors byte-for-byte, or it has drifted |
| SDK errors map to the unified model | `scsdk` / `cloudsdk` | A non-2xx `{Code,Message}` body becomes the business code + HTTP status, never a silent swallow |
| Proto changes are additive within a major version | `proto-hub` | A field-number removal/retype is a CI failure (R14); deprecation is notice, not a break licence |
| ZONAL products require an AZ at quote time | `svc-catalog` quote gate | A VM pinned to one AZ must have its zone chosen before an order is created; discovering it at provisioning is the expensive place to learn it |
| Even spread across 2 AZs does not survive AZ loss | `topology.SurvivesAZLoss` | The larger zone always holds ≥ half; MySQL MGR uses a majority rule, and 2-AZ survival of either zone needs 3 AZs (P3), not (2,1) |
| Stateful workloads carry cross-AZ placement | `middleware` manifests + `check-topology-spread` | A Kafka/MySQL/Redis "surviving AZ loss" by claim but not by manifest is a regression that is silent until an AZ actually fails |
| New products pass the MockDriver 验收门槛 | `rc-eci` rc-demo | Every M-7 product must clear dispatch→adjudicate→meter→suspend→compensate on the MockDriver before launch (06§6.3) — a product that only works on a real cluster cannot be launched |
| Billing granularity is a catalogue row, not a code change | `svc-catalog` quote path | The `DurationUnit` is derived from the matched pricing rule (`PricingRule.Active`/`Specificity`), so SCECI's per-second rule needs no quote-path branch — a new granularity (e.g. per-minute) is one catalogue row |
| Scaling is deny-first + cooldown-gated | `autoscaling.Evaluate` | No matching rule → no-op (never scale on unknown state); a metric spike can't trigger 5 scales in a minute — cooldown prevents flapping |
| Backup retention is enforced, never unbounded | `backup.ExpiredSnapshots` | A policy with no retention is a config smell, not a license to grow forever; expired snapshots are returned for deletion by the reconcile loop |
| The policy simulator shares the engine's Deny-first | `authz.Simulate` | An explicit Deny wins; absent any match → default_deny — the simulator's default is production's default, so a mis-scoped policy is caught at design time, not at the first customer-data incident |
| Only IAM and billing are global; every other service is regional | `multiregion.Classify` | A service mis-placed as global cannot fail over regionally; an unknown service name is rejected, not silently defaulted to REGIONAL |
| The standby region takes no writes and must be >300km away | `multiregion.Plan.Validate` | 00§4.5: P3 不承诺异地多活写 — a writable standby would be the unitization architecture, a separate project; a sub-300km "remote" site is not 异地 |
| Kafka topics are rebuilt per region, never mirrored | `event` + `multiregion.ClassifyState` | 00§4.5: cross-region Kafka mirroring is explicitly not done; the standby rebuilds topics by the cloud.* conventions — mirroring would silently fork the topic contract |
| Region names come from one source, not two | `identifier.IsValidRegion` | `multiregion` delegates region-name validation exactly as `topology` delegates AZ names — a forked regex drifts and rejects a valid region at failover time |
| A settlement split sums back to the gross exactly | `settlement.Settle` | The platform share is derived by subtraction, never a second rounded computation — two independent roundings could disagree, which is a reconciliation defect |
| Temporary credentials always expire | `sts.Issuer` | A zero/negative TTL is rejected at construction; expiry is an inclusive boundary — a non-expiring token is a permanent key wearing a temporary label |
| A flat usage baseline still detects anomalies, without ±Inf | `anomaly.Detect` | stddev 0 makes a z-score undefined; any deviation from a constant stream is the anomaly signal, reported with a finite sentinel score |
| Budget exhaustion freezes the domain's feature releases | `slo.BudgetPolicy` | 08§10.3: remaining <50% slows releases, 0 freezes non-reliability work — the policy is a function of remaining budget, not of operator mood |

## Verification status

| Component | Verified how |
|---|---|
| `pkg-go/*` (33 packages) | `go test` — 505 top-level tests: unit, golden vectors, contention, negative cases |
| Phase-3 services (svc-marketplace, alert-center) | `go build` + `go vet` clean; `go test` green (marketplace 9, alert-center 4); svc-metering anomaly-scan + svc-iam STS endpoint build/vet/test green over real HTTP |
| Phase-3 Terraform Provider (`sdk/terraform`) | Source-only: complete source + static consistency, not built (framework not vendored, see README Verification status) |
| Phase-2 billing forms (M-4) | `go test` (reservepack/spot/invoice); svc-billing + console-bff smoke-tested over real HTTP end-to-end (purchase → settle → 资源包 rank-0 deduction → 红冲 → cost-analysis) |
| Phase-2 OpenAPI ecosystem (M-5) | `go test` (scsdk: signs-via-cps1 + error model); Python `pytest` 12 tests (7 golden-vector sigs byte-match); Explorer smoke-tested over real HTTP (signature matches golden-vector body hash, missing-account 403); `check-proto-breaking` negative-tested |
| Phase-2 dual-AZ topology (M-6) | `go test` (topology: 19 tests incl. the even-spread-can't-survive invariant + MGR majority math); svc-catalog placement + ZONAL quote gate smoke-tested over real HTTP (ZONAL-no-zone 400, zone/region mismatch 400, valid-zone 200); `check-topology-spread` negative-tested (stripped Redis shard caught); cross-AZ middleware manifests structurally validated |
| Phase-2 product matrix (M-7) | rc-{eci,lb,autoscaling,backup,redis} `go run ./cmd/rc-demo` — each passes the 06§6.3 MockDriver gate; `pkg-go` autoscaling + backup domain tests; svc-catalog 21 tests (incl. placement/quote for all new products — REGIONAL sclb/scas/scbackup ignore zone, ZONAL scredis enforces + accepts prepaid); svc-iam web-auth 7 RAM tests + authz 21 tests; real-HTTP e2e for all products (placement → quote → order → pay → fulfill → RUNNING, 5 products coexisting) + RAM simulator (allow/default_deny/explicit_deny-wins/404/400) |
| svc-iam verify endpoint | Wire-level e2e over real HTTP, 7 checks |
| Commercial loop | 7 runnable demos, 50+ asserted scenarios |
| Go services & scaffold | `go build` + `go vet` clean; 8 new servers smoke-tested over HTTP |
| Frontend (phase 2) | `pnpm -r typecheck` all 18 projects pass; devops-explorer (M-5) + console-eci/lb/autoscaling/backup/redis (M-7) + web-account RAM simulator (M-7.6) added in phase 2 |
| Proto IDL (17 packages) | `buf`-style source validation (syntax + go_package, no java) — `buf lint`/`breaking` not run (buf not installed); `tools/check-proto-breaking.py` guards wire-breaking changes source-only |
| APISIX plugins & routes | Static validators only — **not executed** |
| Vitess (vtgate/vttablet) | Documented & manifest-validated only — **not deployed** (no MySQL/cluster on this host) |
| Helm charts / K8s manifests | Parsed and structurally checked — **not applied** |
| Cross-AZ middleware (M-6) | Manifests structurally validated + topology-spread checked — **not applied** (no cluster); `topology.SurvivesAZLossMGR` proves the MGR (2,1) boundary the runbook drills |
| SQL DDL | **Not executed** (no MySQL on this host) — incl. phase-2 V2/V3 (resource_pack, invoice) + M-6 V2 resource_db az_topology |

The bottom four rows are source-only by choice (the unified Go stack agreed at the
start). They are complete, internally consistent, and statically validated — but
nothing has run them. Vitess was selected to replace ShardingSphere-JDBC as the
MySQL sharding layer (per 04§6/10-research §4.5); it is captured in the docs and
manifest-validated but not deployed on this host.

**Race detector**: `go test -race` needs cgo, unavailable on this Windows host.
Concurrency guarantees are covered by contention tests instead —
`ledger.TestDoubleSpendUnderContention` (50 charges against a balance admitting
10) and `quota.TestConcurrentOccupyCannotOversell` (50 reservations against a
limit of 20). Both assert survivor count, final state, and reconciliation agree.

**Docker is installed in WSL2 (Ubuntu 24.04) and the daemon is running**, but the
WSL user is not in the `docker` group, so containers cannot be started from this
session. Adding it unblocks real verification of the bottom four rows:

```sh
wsl -d Ubuntu-24.04 -e sudo usermod -aG docker $USER   # then: wsl --shutdown
```

With that, MySQL can execute the DDL, APISIX can load the Lua plugins, and kind
can apply the Helm charts. gcc is present in WSL, so installing Go there also
enables `-race`.

## What phase 1 does not include

Deferred by explicit decision, not oversight. Items marked **(done in M-4)**
landed in phase 2; **(done in M-5)** items landed in the OpenAPI ecosystem
milestone; **(done in M-6)** items landed in the dual-AZ milestone; **(done in
M-7)** items in the product matrix. The rest remain phase-2/3 work.

- **SCECI 弹性容器实例** — **(done in M-7.1)** phase 2 (adjudication C1+S1; VPC
  is an ECS prerequisite and ECI depends on advanced K8s scheduling). Registered
  in svc-catalog (per-second POSTPAID), CRD `SceciInstance`, rc-eci operator
  demo passes the 06§6.3 MockDriver 验收门槛, console-eci sub-app wired
- **资源包 / 抢占式计费** — **(done in M-4)** phase 2 (decision D6); the enum
  and deduction-waterfall tier are now live (`pricing.Sellable()` opens all four
  billing forms; `pkg-go/reservepack` backs the pack ledger, `pkg-go/spot` the
  floating price engine)
- **发票 / 红冲** — **(done in M-4)** phase 2 (09-roadmap B6); `pkg-go/invoice`
  issues and 红冲-reverses tax documents
- **OpenAPI Explorer + Go/Python SDK + API 版本化** — **(done in M-5)** phase 2
  (09-roadmap M-5); `pkg-go/scsdk` (Go) + `sdk/python/cloudsdk` (Python) sign
  with cps1 and parse errors via errorsx; svc-api-meta powers the Explorer; the
  golden-vector fixture pins cross-language signature consistency
- **同城双活 (P2)** — **(done in M-6)** phase 2; the reserved region/AZ model
  is now an implemented P2 contract (`pkg-go/topology` failover math,
  ZONAL quote gate, cross-AZ middleware manifests, drill runbook). 两地三中心
  (P3) remains phase 3
- **KubeVirt VM driver** — phase 2 (decision R-03); `provision.VMDriver`
  returns `ErrDriverNotReady` rather than silently doing nothing
- **满减券 / 折扣券** — phase 2; phase 1 ships 定额代金券 only, as the carrier
  for 免费试用 (decision D7)
- **RAM 子账号进阶 / STS 产品化** — **(done in M-7.6)** phase 2 (07§10 M1);
  `pkg-go/authz/simulator.go` + svc-iam `/api/ram/{roles,policies,simulate}` +
  web-account RoleList/PolicySimulator; STS remains phase 3
- **SCLB 负载均衡 / 弹性伸缩 / 备份快照 / 托管 Redis** — **(done in M-7)**
  phase 2 (M-7.2/7.3/7.4/7.5): each onboarded via the four-component pattern
  (CRD + rc-* operator passing the MockDriver gate + console-* sub-app);
  `pkg-go/autoscaling` + `pkg-go/backup` ship real domain engines
