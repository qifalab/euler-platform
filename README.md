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

## Repository layout

```
docs/architecture/        Spec (binding source of truth; chapter 00-11)
docs/cps1-implementation-notes.md   CPS1 contract notes (encoding, verify order)
docs/apisix-gateway-notes.md        Gateway chain, exemptions, fail-closed rules
proto-hub/                IDL single source of truth (buf lint + breaking)
  proto/starcloud/{iam,event,catalog,order,payment,billing,metering,
    orchestrator,quota,workflow,monitor,notify,audit,ticket,org,alert,apimeta}/v1/*.proto
  testdata/cps1-golden-vectors.json  Cross-language signature fixture

pkg-go/                   Shared domain libraries (18 packages, 274 tests)
  — identity & access —
  cps1/                   CPS1-HMAC-SHA256 signer/verifier (07§4.1)
  kms/                    Envelope encryption, key hierarchy (07§5.3)
  accesskey/              AK/SK lifecycle: max 2, rotation grace (07§2.2)
  authz/                  RAM policy engine, Deny-first (07§3.3)
  verify/                 Gateway forward-auth chain (07§4.1)
  — commerce —
  pricing/                Fixed-point money + pricing engine (01§7, §12.3)
  order/                  Unified order model + state machine (03§4.2.2)
  ledger/                 Cash balance + append-only journal (S29, 03§8.5)
  — resources —
  resource/               Resource lifecycle + arrears/expiry (03§5.2, 01 D8)
  workflow/               Saga engine with compensation (03§4.3.3, §8.4)
  provision/              ProvisionDriver + CRD conventions (06§4, §6.2)
  quota/                  Two-phase reservation with TTL (03§4.3.2)
  — billing —
  metering/               Hourly aggregation + covered_ratio (05§5)
  billing/                Deduction waterfall + reconciliation (03§4.2.4)
  — support —
  notify/                 Delivery evidence for trust-critical classes (03§4.4.2)
  audit/                  Tamper-evident hash chain (07§6)
  — platform —
  identifier/             Global identifier conventions (00 附录A)
  event/                  Kafka envelope + topic constants (04§5.4)
  errors/                 Unified OpenAPI error model (03§9.3)

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
  svc-api-meta/           OpenAPI 元数据/文档/SDK 中心 (03§4.5.1)
  console-bff/            控制台聚合层 (03§3)
  rc-compute/             数据面控制器: ProvisionDriver + SCECS CRD
  (rc-storage/rc-network/rc-database 的 Helm chart 已就绪,控制器代码复用 rc-compute 模式)

platform/frontend/        Vue+Wujie 官网/控制台骨架 (source-only, 02章)
  apps/site/              营销官网: 7 个一期产品
  apps/console-base/      Wujie 微前端基座 + 8 个子应用注册表
  apps/console-ecs/       示例子应用 (mount/unmount 生命周期)
  docs/                   02 章要点摘录

deploy/gitops-manifests/  ArgoCD App-of-Apps; dir-as-env, single trunk (08§4.3)
  apps/<svc>/             per-service Helm chart (env-agnostic templates)
  envs/{dev,staging,prod}/values-overrides/   env differences ONLY here
  platform/apisix/        Gateway: plugins (Lua), routes, config, deployment
platform/ci-templates/    Shared GitLab CI pipeline templates
tools/                    Repo validators (YAML, Lua, APISIX route semantics)
```

## Build & test

```sh
cd pkg-go && go test ./...     # 18 packages, 274 tests
```

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
```

The route validator catches failures that are silent in production: a route
missing `sc-auth` is an unauthenticated endpoint; a Nacos subscription string in
the wrong form resolves to zero upstreams and 502s with nothing in the logs. It
was negative-tested against eight deliberately broken routes and caught all of
them.

## Single-source-of-truth cross-references

- **Signature algorithm**: `pkg-go/cps1` (07§4.1, D4) — SDK, gateway verifier,
  OpenAPI Explorer and doc-site examples share one impl, pinned by
  `proto-hub/testdata/cps1-golden-vectors.json`. See
  `docs/cps1-implementation-notes.md`.
- **Global identifiers**: `pkg-go/identifier` (00 附录A).
- **Kafka topics**: `pkg-go/event` (04§5.4) — declared once, referenced by symbol.
- **Error codes**: `pkg-go/errors` (03§9.3).
- **RAM policy semantics**: `pkg-go/authz` (07§3.3) — Deny-first, fail-closed on
  unknown condition operators.

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

## Verification status

| Component | Verified how |
|---|---|
| `pkg-go/*` (18 packages) | `go test` — 274 tests: unit, golden vectors, contention, negative cases |
| svc-iam verify endpoint | Wire-level e2e over real HTTP, 7 checks |
| Commercial loop | 7 runnable demos, 50+ asserted scenarios |
| Go services & scaffold | `go build` + `go vet` clean; 8 new servers smoke-tested over HTTP |
| Proto IDL (17 packages) | `buf`-style source validation (syntax + go_package, no java) — `buf lint` not run (buf not installed) |
| APISIX plugins & routes | Static validators only — **not executed** |
| Vitess (vtgate/vttablet) | Documented & manifest-validated only — **not deployed** (no MySQL/cluster on this host) |
| Helm charts / K8s manifests | Parsed and structurally checked — **not applied** |
| SQL DDL | **Not executed** (no MySQL on this host) |

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

Deferred by explicit decision, not oversight:

- **SCECI 弹性容器实例** — phase 2 (adjudication C1+S1; VPC is an ECS
  prerequisite and ECI depends on advanced K8s scheduling)
- **资源包 / 抢占式计费** — phase 2 (decision D6); the enum reserves them and
  the deduction waterfall has the tier, but they are not sellable
- **KubeVirt VM driver** — phase 2 (decision R-03); `provision.VMDriver`
  returns `ErrDriverNotReady` rather than silently doing nothing
- **满减券 / 折扣券** — phase 2; phase 1 ships 定额代金券 only, as the carrier
  for 免费试用 (decision D7)
- **RAM 子账号进阶 / STS 产品化** — phase 2 (07§10 M1)
- **同城双活 (P2) / 两地三中心 (P3)** — phase 2 and 3; the model carries
  region/AZ from day 1, deployment is single-AZ
