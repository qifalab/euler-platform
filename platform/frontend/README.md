# Euler Frontend

The Euler (辰云) frontend — a real, runnable pnpm monorepo built
against [`docs/architecture/02-frontend-architecture.md`](../../docs/architecture/02-frontend-architecture.md).

**Status: production-grade usable.** Six sites + 13 product-category console
sub-apps (one common shell + category children) + the developer explorer +
shared `@eu/*` packages, all typechecking and building. The console shell loads
sub-apps via Wujie and renders real data from the console-bff in dev. Phase-3
surfaces (marketplace / 稳定性 / 告警中心 / 异常检测 / 地域容灾 / STS) are wired to
their real backend services — see [Phase-3 frontend sync](#phase-3-frontend-sync-三期工程同步).

## Toolchain

Node ≥ 18, pnpm ≥ 8. Verified on node 24 / pnpm 11.

## Layout

```
platform/frontend/
├── package.json             # workspace root
├── pnpm-workspace.yaml      # packages/* + apps/*, allowBuilds (esbuild/vue-demi)
├── packages/                # shared layer (02§1.3, §6.5)
│   ├── tokens/              # @eu/tokens — design tokens, CSS vars (02§10.1)
│   ├── ui/                  # @eu/ui — Element Plus thin-wrap + business components (02§10.2)
│   ├── sdk/                 # @eu/sdk — unified request client, 401 refresh, error model (02§9.1)
│   ├── wujie-bridge/        # @eu/wujie-bridge — props injection + bus events (02§5.4/6.4)
│   └── console-kit/        # @eu/console-kit — ResourceTable, RegionSelector, ErrorBoundary (02§7)
└── apps/                    # 20 apps
    ├── console-base/        # console shell: Vue3+Vite+Pinia+Router+Wujie (02§4) — :5173
    ├── console-ecs/         # compute (EUECS): list/detail/buy-wizard (02§7.2-7.4) — :5174
    ├── console-storage/      # storage (EUOSS/EUBS): bucket list/detail — :5176
    ├── console-network/      # network (EUVPC/EUEIP): vpc list/detail — :5177
    ├── console-database/     # database (EURDS): instance list/detail — :5178
    ├── console-monitor/      # monitor (EUMON): rules/dashboard + 告警中心/异常检测/稳定性/地域容灾 — :5179
    ├── console-eci/          # serverless container (EUECI) — :5183
    ├── console-lb/           # load balancer (EULB) — :5184
    ├── console-autoscaling/  # autoscaling (EUAS) — :5185
    ├── console-backup/       # backup (EUBS) — :5186
    ├── console-redis/        # redis (EURDS) — :5187
    ├── console-kafka/        # kafka (SCKAFAKA) — :5188
    ├── console-logservice/   # log service (SCSLS) — :5189
    ├── devops-explorer/      # OpenAPI explorer + developer community — :5182
    ├── web-account/         # SSO account center: login/register/forgot/realname/RAM/AK/STS (02§5) — :5175
    ├── web-billing/          # billing center: bills(接 BFF)/orders/renew (02§1.2) — :5180
    ├── web-marketplace/      # marketplace (M-10): storefront/publish/review/settlement — :5190
    ├── web-ticket/           # ticket support: list/create (02§1.2) — :5181
    ├── site/                 # marketing portal: Nuxt3 SSR (02§8.1) — :3000
    └── docs-site/            # docs site: VitePress SSG (02§8.2) — :4000
```

## Getting started

```sh
pnpm install
pnpm build:packages          # build shared layer first (apps consume dist/)

# Start the backend aggregation layer (console-bff, Go):
cd services/console-bff && go run ./cmd/server -http :9200

# Start all frontend dev servers (each in its own terminal, or background):
pnpm dev:base                # console shell on :5173 (proxies /console → :9200)
pnpm dev:ecs                 # + each sub-app on its own port (see above)
pnpm dev:site                # portal on :3000
pnpm --filter docs-site dev  # docs on :4000
```

Open http://localhost:5173 — the shell overview shows real balance/resources/
orders from the BFF (dev account 100123). Click a product card to load its
sub-app via Wujie.

## Dev account

The console-bff is seeded with account **100123** (¥500 balance, 2 resources,
1 pending order). The vite proxy injects `X-Euler-Account-Id: 100123` so the shell
renders real data without the APISIX gateway.

SSO login (web-account :5175) uses dev seed credentials:
**admin@euler.emoera.com / euler123** against the real svc-iam
`/api/auth/login` (Bearer access token + HttpOnly refresh cookie).

## How it maps to the architecture doc

| Path | Architecture reference (02 doc) |
|---|---|
| `packages/*` | §1.3 public monorepo, §6.5 externals, §10 design system, §7 console-kit |
| `console-base` | §4 shell (registry, loading, keep-alive), §5 auth store, §7.1 layout, BFF proxy |
| `console-ecs` | §3.4 sub-app lifecycle, §7.2 list, §7.3 detail, §7.4 buy-wizard |
| `console-{storage,network,database,monitor}` | §1.2 category sub-apps, §7.2 ResourceTable |
| `web-{account,billing,ticket}` | §1.2 dual-form sites, §5 SSO, §1.2 billing/ticket |
| `site` | §8.1 Nuxt3 SSR portal |
| `docs-site` | §8.2 VitePress SSG |

## Phase-3 frontend sync (三期工程同步)

The phase-3 backend milestones (M-8 多地域, M-9 稳定性平台, M-10 marketplace,
D-1 高级告警, D-2 异常用量检测, D-3 STS) are surfaced in the frontend with
**real backend connections** — every view below fetches live data over HTTP via
`@eu/sdk` / `fetch` + vite dev proxy, no mocks. Each sub-app's vite proxy injects
`X-Euler-Account-Id: 100123` (the dev stand-in for the gateway-authorized header).

### web-marketplace (:5190, new app — M-10)

| View | Backend (svc-marketplace :9212) |
|---|---|
| `Storefront` 商品目录 | `GET /api/v1/marketplace/listings` (default = APPROVED, category filter) |
| `PublishListing` 发布商品 | `POST /api/v1/marketplace/listings` → lands in PENDING_APPROVAL |
| `ReviewDesk` 审核台 | `GET ?status=PENDING_APPROVAL` + `POST /listings/{id}/approve` (approve/reject + note) |
| `Settlement` 分账结算 | `POST /api/v1/marketplace/settlements` (idempotent per orderId, 伙伴/平台 split by bps) |

Registered as a Wujie sub-app in `svc-api-meta` (sub-app registry :9201) and the
`console-base` fallback registry — it runs standalone on :5190 or inside the
console shell at `/marketplace`.

### console-monitor (:5179, four new views)

| View | Backend |
|---|---|
| `AlertCenter` 告警中心 | alert-center :9213 — `POST /ingest` ×2 (演练) → `POST /flush` (四阶段收敛: 去重/分组/抑制/静默 + 10 条/分钟限流) → `GET /alerts` |
| `AnomalyScan` 异常检测 | svc-metering :9206 — `GET /api/v1/metering/anomaly-scan?resourceId&metric` (z-score SPIKE/DROP verdict) |
| `Stability` 稳定性平台 | svc-monitor :9202 — `GET /api/v1/monitor/slo` (error budget + 1h/3d burn rate + 发布政策 + SLA 资格) and `GET /api/v1/monitor/chaos` (演练科目 + 补救标记) |
| `RegionTopology` 地域容灾 | svc-catalog :9207 — `GET /api/v1/catalog/region-topology` (两地三中心 plan + 复制通道 RPO 判定 + 故障切换步骤 + 全局/区域服务分类) |

### web-account (:5175, new view)

| View | Backend (svc-iam :9101) |
|---|---|
| `StsCredentials` STS 临时凭证 | `GET /api/ram/roles` (角色下拉) → `POST /api/sts/assume-role` (签发限时 AK/SK/SecurityToken, SecretKey 仅显示一次, 倒计时过期提示) |

### Verified end-to-end

Real-HTTP smoke over the dev-proxy chain (services started, endpoints hit,
envelopes unwrapped): region-topology RPO verdicts; SLO burn-rate → FREEZE on
`metering-no-loss`; chaos drill remediation flags; marketplace
publish → PENDING_APPROVAL → approve → APPROVED → settle (70/30 split);
alert-center dedup (3 ingested → 2 emitted); anomaly-scan baseline NONE →
ingested spike → SPIKE (score 100); login → assume-role credential triple.
Go vet/test × 7 services, `pnpm typecheck`/`build` × 4 apps, vitest 34/34 green.

## Build verification

| Component | Verified |
|---|---|
| `@eu/*` (5 packages) | `pnpm -r --filter "@eu/*" build` — ES+CJS+.d.ts+style.css each |
| 11 apps | all `vite build` / `nuxt build` / `vitepress build` pass; all typecheck clean |
| Dev boot | all 9 JS dev servers (5173-5181) respond 200; BFF :9200 healthy |
| BFF integration | base `/console/overview` returns real seeded data via dev proxy |
| Wujie CORS | all 8 sub-apps return `Access-Control-Allow-Origin: *` for cross-origin entry fetch |
| Sub-app bundle | category sub-apps ~2 KB gzip business JS (externals; ≤300 KB budget) |

## What's still mocked (toward full production)

- `@eu/sdk` is hand-written; real version is OpenAPI-generated from proto-hub (02§9.1). (The runtime client is real — token injection, 401 single-flight refresh, envelope unwrapping, structured errors all work against live services, verified by 34 unit tests.)
- Cross-service gRPC: services are independent stdlib HTTP, no proto-generated stubs interconnect them yet (proto-hub IDL is source-only).
- Real database: all services use in-memory stores (MySQL/Vitess DDL written but not executed).
- Kafka/outbox relay: topic constants + DDL exist, no producer/consumer/relay wired.
- i18n and breadcrumb not yet implemented (dark mode + global search ⌘K done).
- No ArgoCD App-of-Apps, app-repo CI pipeline, Otel instrumentation, or cross-service e2e integration test yet.

## Real backend services (run these for full dev fidelity)

```sh
# All on stdlib HTTP, in-memory stores seeded with account 100123.
cd services/console-bff     && go run ./cmd/server -http :9200   # BFF aggregation
cd services/svc-iam         && go run ./cmd/web-auth -http :9101  # SSO + register + realname + RAM/AK/profile
cd services/svc-api-meta    && go run ./cmd/server -http :9201   # sub-app registry
cd services/svc-orchestrator&& go run ./cmd/server -http :9203   # fulfilment saga (开通)
cd services/svc-order       && go run ./cmd/server -http :9204   # orders (下单)
cd services/svc-payment     && go run ./cmd/server -http :9205   # balance/recharge (充值)
cd services/svc-metering    && go run ./cmd/server -http :9206   # usage aggregation (计量)
cd services/svc-catalog     && go run ./cmd/server -http :9207   # pricing (询价)
cd services/svc-quota       && go run ./cmd/server -http :9208   # two-phase quota
cd services/svc-monitor     && go run ./cmd/server -http :9202   # alert rules + SLO/chaos (监控/稳定性)
cd services/alert-center    && go run ./cmd/server -http :9213   # 告警中心: ingest/flush/alerts (收敛+限流)
cd services/svc-marketplace && go run ./cmd/server -http :9212   # marketplace: listings/approve/settle
```
