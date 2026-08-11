# StarCloud Phase-1 Frontend Scaffold

Source-only scaffold for the StarCloud (辰云) phase-1 frontend, mirroring the
intended structure described in
[`docs/architecture/02-frontend-architecture.md`](../../docs/architecture/02-frontend-architecture.md).

> **IMPORTANT: This is a static, source-only scaffold.**
> No `node_modules` are committed (see the root `.gitignore`), no lockfile is
> required, and nothing here is a finished product. A frontend team builds on
> these files as the starting skeleton — actual dependencies, build tooling,
> design tokens and real business pages are intentionally out of scope.

## Layout

```
platform/frontend/
├── README.md                # this file
├── package.json             # workspace root: @cloudplatform/*
├── pnpm-workspace.yaml      # pnpm workspaces (console sub-apps)
├── docs/
│   └── frontend-architecture-notes.md   # key pointers into 02-frontend-architecture.md
└── apps/
    ├── console-base/        # Wujie micro-frontend base (main/console-shell analog)
    │   ├── index.html
    │   └── src/
    │       ├── main.js      # registerMicroApps + mount sub-apps
    │       └── App.vue      # shell layout with sub-app container
    ├── console-ecs/         # minimal compute sub-app (console-compute analog)
    │   └── src/
    │       ├── App.js       # sub-app root component
    │       └── index.js     # exports Wujie mount/unmount lifecycle
    └── site/                # marketing portal (web-portal analog, static)
        ├── index.html
        └── assets/css/main.css
```

## How it maps to the architecture doc

| Scaffold path | Architecture reference (02 doc) |
|---|---|
| `apps/console-base` | Console Shell / Wujie base (`console-shell`) |
| `apps/console-ecs` | Compute category sub-app (`console-compute`, SCECS) |
| `apps/site` | Marketing portal (`web-portal`, static stand-in for Nuxt3 SSR) |

The full console sub-app set from the architecture doc (registered by the base)
is: `console-ecs`, `console-oss`, `console-rds`, `console-vpc`, `console-monitor`,
`console-billing`, `console-iam`, `console-ticket` (see `console-base/src/main.js`).

## Getting started (for the frontend team)

1. `pnpm install` — installs workspace deps (run only if/when you add real deps).
2. Serve the static pages (e.g. `npx serve apps/console-base`) to preview.
3. Replace the placeholder sub-app entries (`apps/console-ecs`) with real
   business modules, add the remaining category sub-apps, and wire up the
   `@sc/*` shared packages from the architecture doc.

## References

- Architecture: `docs/architecture/02-frontend-architecture.md`
- Product catalog: `docs/architecture/01-product-catalog.md`
- Notes digest: `docs/frontend-architecture-notes.md`
