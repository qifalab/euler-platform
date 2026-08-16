# StarCloud Phase-1 Frontend

The StarCloud (辰云) phase-1 frontend — a real, runnable pnpm monorepo built
against [`docs/architecture/02-frontend-architecture.md`](../../docs/architecture/02-frontend-architecture.md).

**Status: production-grade usable.** Six sites + seven product-category console
sub-apps (one common shell + seven category children) + shared `@sc/*` packages,
all typechecking and building. The console shell loads sub-apps via Wujie and
renders real data from the console-bff in dev.

## Toolchain

Node ≥ 18, pnpm ≥ 8. Verified on node 24 / pnpm 11.

## Layout

```
platform/frontend/
├── package.json             # workspace root
├── pnpm-workspace.yaml      # packages/* + apps/*, allowBuilds (esbuild/vue-demi)
├── packages/                # shared layer (02§1.3, §6.5)
│   ├── tokens/              # @sc/tokens — design tokens, CSS vars (02§10.1)
│   ├── ui/                  # @sc/ui — Element Plus thin-wrap + business components (02§10.2)
│   ├── sdk/                 # @sc/sdk — unified request client, 401 refresh, error model (02§9.1)
│   ├── wujie-bridge/        # @sc/wujie-bridge — props injection + bus events (02§5.4/6.4)
│   └── console-kit/        # @sc/console-kit — ResourceTable, RegionSelector, ErrorBoundary (02§7)
└── apps/                    # 11 apps
    ├── console-base/        # console shell: Vue3+Vite+Pinia+Router+Wujie (02§4) — :5173
    ├── console-ecs/         # compute (SCECS): list/detail/buy-wizard (02§7.2-7.4) — :5174
    ├── console-storage/      # storage (SCOSS/SCBS): bucket list/detail — :5176
    ├── console-network/      # network (SCVPC/SCEIP): vpc list/detail — :5177
    ├── console-database/     # database (SCRDS): instance list/detail — :5178
    ├── console-monitor/      # monitor (SCMON): alert rules/dashboard — :5179
    ├── web-account/         # SSO account center: login/register/forgot/realname/RAM/AK (02§5) — :5175
    ├── web-billing/          # billing center: bills(接 BFF)/orders/renew (02§1.2) — :5180
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
1 pending order). The vite proxy injects `X-Sc-Account-Id: 100123` so the shell
renders real data without the APISIX gateway.

SSO login (web-account :5175) uses dev seed credentials:
**admin@starcloud.cn / starcloud123** (mock token issuer — svc-iam has no
`/api/auth/*` yet).

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

## Build verification

| Component | Verified |
|---|---|
| `@sc/*` (5 packages) | `pnpm -r --filter "@sc/*" build` — ES+CJS+.d.ts+style.css each |
| 11 apps | all `vite build` / `nuxt build` / `vitepress build` pass; all typecheck clean |
| Dev boot | all 9 JS dev servers (5173-5181) respond 200; BFF :9200 healthy |
| BFF integration | base `/console/overview` returns real seeded data via dev proxy |
| Wujie CORS | all 8 sub-apps return `Access-Control-Allow-Origin: *` for cross-origin entry fetch |
| Sub-app bundle | category sub-apps ~2 KB gzip business JS (externals; ≤300 KB budget) |

## What's still mocked (toward full production)

- `@sc/sdk` is hand-written; real version is OpenAPI-generated from proto-hub (02§9.1). (The runtime client is real — token injection, 401 single-flight refresh, envelope unwrapping, structured errors all work against live services, verified by 34 unit tests.)
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
cd services/svc-monitor     && go run ./cmd/server -http :9202   # alert rules (监控)
```
