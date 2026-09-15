# Frontend Architecture — Key Notes (digest of 02-frontend-architecture.md)

This file is a pointer/notes digest of
[`docs/architecture/02-frontend-architecture.md`](../../../docs/architecture/02-frontend-architecture.md)
(v1.4). It is NOT the source of truth — read the full chapter for details.

## Site structure (six independent sub-domains, per 01-product-catalog D5)

| Site | Domain | Role |
|---|---|---|
| Marketing portal | `www.euler.emoera.com` | Nuxt3 SSR (SEO) |
| Docs site | `docs.euler.emoera.com` | VitePress SSG |
| Console shell | `console.euler.emoera.com` | SPA (Vue3 + Vite), Wujie base |
| Account center | `account.euler.emoera.com` | SSO-only auth domain |
| Billing center | `billing.euler.emoera.com` | Billing / orders |
| Ticket support | `ticket.euler.emoera.com` | Work orders / support |

Console product sub-apps are split **by product category** (not one-per-product),
per 01-product-catalog D9: compute / storage / network / database / middleware /
monitor / security — 7 category sub-apps plus 3 dual-form sites (account /
billing / ticket).

## Micro-frontend choice: Wujie (无界)

- iframe-level JS sandbox + WebComponent mount: strong JS/CSS isolation, no CSS patching.
- Native `alive` mode for keep-alive (switch products without losing form/list state).
- Vite/Vue3 zero-adaptation: sub-apps are standard Vite apps, entry = built `index.html`.
- Sub-apps must work standalone (independent run) — used as degradation path.

## Console shell (base)

- Registry (`svc-api-meta` `GET /api/v1/meta/console/apps`) supplies sub-app entries at
  runtime; front-end caches in sessionStorage (5 min), falls back to a built-in
  `fallback-registry.json`.
- Base keeps: top bar, product menu, breadcrumb container, global region switcher
  (`regionId`, e.g. `cn-north-1`), auth store, global search, 404 / error pages.
- Sub-apps register their menu / breadcrumb via the bridge.
- Route: first path segment = productCode (e.g. `/euecs/instances`); sub-app owns the rest.

## Auth (SSO)

- Dual token: short-lived `access_token` (JWT, 15 min, memory-only) + long-lived
  `refresh_token` (opaque, 7 days, HttpOnly cookie, `Domain=.euler.emoera.com`).
- Console / billing / ticket share session via root-domain cookie; silent refresh
  on access_token expiry or 401.
- Route guard: whitelist -> silent refresh -> permission check (productCode action).

## Console Kit (`@eu/console-kit`)

- Shared building blocks: `ResourceTable`, `CreateWizard`, `StatusBadge`,
  `RegionSelector`, `PriceText`, `EmptyGuide`.
- Global conventions: unified status badges, requestId on all errors, skeleton
  first-load loading (no full-screen spinners), empty states always give a next step.

## Shared packages (`@eu/*`, private registry)

`@eu/ui`, `@eu/tokens`, `@eu/sdk`, `@eu/console-kit`, `@eu/wujie-bridge` —
consumed by version, built via externals sharing (no runtime Module Federation
for phase 1).

## Design tokens (`@eu/tokens`)

CSS variables `--eu-*`: color/brand, bg, text, font-size, spacing, radius/shadow,
layout (`--eu-topbar-height: 56px`, `--eu-sider-width: 208px`). Dark mode via
`[data-theme="dark"]`. Base component library: Element Plus, thin-wrapped by `@eu/ui`.

## Performance budgets (CI-enforced)

- Shell first-screen JS <= 350 KB (gzip)
- Single sub-app business JS (excl. shared) <= 300 KB
- Shared layer cache <= 450 KB
- Sub-app switch (pre-exec hit) < 600ms

## Engineering

- TypeScript strict, pnpm workspaces, Node LTS, Conventional Commits,
  Pinia (single instance), requests only via `@eu/sdk`, i18n zh-CN first.
- Env config NOT baked into build: delivered at runtime via
  `GET /api/v1/meta/client-config` + `window.__SC_ENV__`.

## Roadmap note (P0/P1)

P0: public monorepo, console-shell + Wujie base + registry, `web-account` SSO,
`console-storage`/`console-compute` skeletons + `web-billing`/`web-ticket`,
portal home + 3 product pages + pricing. P1: console-kit mature,
`console-network`/`console-database`/`console-monitor`.
