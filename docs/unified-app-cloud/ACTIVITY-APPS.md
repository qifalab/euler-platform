# Native Statistics and Lottery

These modules are independent Euler implementations. The source projects remain unchanged. Both use the platform SQLite database, Euler OIDC session, CSRF checks, project permissions and transactionally recorded audit events. No Express, Next.js, original database or legacy administration token is used.

## Feature coverage

| Product and original source | Baseline behavior | Native Euler implementation |
|---|---|---|
| Statistics `routes/index.js`, `views/index.pug` | Landing page and integration instructions | Application workspace and **接入与计数器** with copyable, live site-specific URLs |
| `public/js/stats.js`, `routes/stats.js` | Persistent visitor ID; URL/referrer/title/resolution/language collection; retries | Real `tracker.js`, per-site visitor ID, automatic page-load collection, bounded retries; `window.eulerTrackPage()` for SPA navigation |
| `views/admin.pug`, `public/js/admin.js` | All-time PV, UV, distinct pages, URL ranking, last visit, pagination, 60-second refresh | **访问概览** with all metrics, 10/20/50/100 pagination, auto refresh and explicit refresh |
| `public/js/page-stats.js` | Page-count badge, refreshed every 60 seconds | Real `widget.js`, optional existing `#page-view-counter`, cross-origin site-scoped page-count API |
| `public/js/simple-page-stats.js` | Callback/Promise page count | Real `helper.js` exposes `window.getPageViews(callback?)` |
| Original token login and one global database | Global token in query/header; no sites or tenant boundary | Euler login replaces token login; project-owned sites have names, precise allowed domains, enabled state and public collection IDs |
| Lottery `src/app/page.tsx`, `Navbar` | Create/revisit rooms; browser-local room list | Persistent project activity list, search, explicit creation, revisit from any device |
| `room/[roomId]/page.tsx` | Participant/round/winner counters, default 5-second refresh | Host workspace, toggleable polling, manual refresh, unambiguous activity title and project shell |
| `QRCodeGenerator`, `register/page.tsx` | QR invitation, copied signup URL, mobile name/department form, signup again | Local QR generation, narrow invitation token, responsive public form, success and repeat-registration flow; token rotation and pause supported |
| `/api/users`, `/api/users/manual` | Read, public/manual add, batch numbered users skipping duplicates, delete | Authenticated paginated/searchable participant list, manual add, batch generation, explicit removal; public signup is a separate route |
| `/api/lottery` POST | Count, prevent repeat wins toggle, server rounds, optional API prize name | Transactional unbiased random sampling, count validation, exact idempotent retries, prize-name UI, persisted rounds and settings |
| Room page result modal/current and all-winner tables | Animated celebration, current display, winner history, local clear | Animated accessible result dialog with reduced-motion support; current results restored from server; display-only clear; complete paginated history |
| `/api/history`, `history/page.tsx` | Room/round grouped winner lists, prize/time/name/department, filtering and pagination | Server-filtered, project-scoped history across all rooms or selected room; persistent ownership replaces localStorage |
| `/api/lottery` PUT | Clear room's winning state while retaining participants | **开启新抽奖周期**, manage permission and explicit confirmation; participants remain eligible; old cycle retained for audit, excluded from current history |
| `/api/reset-db` | Anonymous full database drop and recreation | Intentionally absent. There is no public or project-facing global reset route |

Source snapshots: Statistics `30ad3c2d7ded5eae059dd1380be91595ddbf1bbc`; Lottery `ab25a69d2678072a3608d36a3a0a2dd8d9971123`. Apache-2.0 licenses and source attribution accompany each backend module. This is a reimplementation of the usable product behavior, not a copy of production configuration.

Neither baseline contains CSV/XLSX export, statistical date-range/source-device reports, participant file import, prize inventory, or a separate fullscreen/music configuration system. Those are not claimed as existing parity. Lottery's existing result celebration is preserved; room settings and prize input make previously hidden or implicit behavior explicit.

## Routes and permissions

Private routes below are relative to `/api/v1/tenants/{tenantID}/projects/{projectID}/apps/{applicationID}`. The platform supplies scope only after validating session, CSRF, membership, installation and permission. Body/query/header tenant or actor IDs cannot override it. Every private object lookup binds ID, tenant, project and installation.

| App | Method/path | Permission |
|---|---|---|
| statistics | `GET /sites` | read |
| statistics | `POST /sites`, `PATCH /sites/{id}` | manage |
| statistics | `GET /sites/{id}/report?page=&pageSize=` | read |
| statistics | `GET /sites/{id}/integration` | read |
| lottery | `GET /rooms`, `GET /rooms/{id}` | read |
| lottery | `POST /rooms`, `PATCH /rooms/{id}` | write |
| lottery | `GET /rooms/{id}/invitation` | write |
| lottery | `POST /rooms/{id}/invitation/rotate` | manage |
| lottery | `GET /rooms/{id}/participants?page=&pageSize=&search=` | read |
| lottery | `POST /rooms/{id}/participants`, `POST /rooms/{id}/participants/batch`, `DELETE /rooms/{id}/participants/{participantID}` | write |
| lottery | `POST /rooms/{id}/draws` | write |
| lottery | `GET /rooms/{id}/draws`, `GET /history?roomId=&page=&pageSize=&search=` | read |
| lottery | `POST /rooms/{id}/reset` with `{confirm: roomID}` | manage |

Public routes have no personnel scope. They are distinct capability-limited data surfaces and recheck the installation's enabled state.

| Public route | Allowed behavior |
|---|---|
| `/public/statistics/sites/{publicID}/tracker.js` | Embed automatic collector on an allowed domain |
| `/public/statistics/sites/{publicID}/widget.js`, `helper.js` | Embed current-page counts, without report access |
| `POST …/collect` | Bounded JSON collection; precise Origin and page-host match; only that site |
| `GET …/page-views?url=` | Count for an allowed URL within that site; no visitor/identity data |
| `/public/lottery/join/{token}` | Mobile signup page; room title/description and aggregate counts only |
| `GET …/info`, `GET …/qr.png` | Same public room summary; locally generated QR image |
| `POST …/register` | Add one name/department to this open room only |

The collector strips URL query strings and fragments at both client and server. Raw visitor IDs and IPs are not persisted; per-site HMACs prevent cross-site correlation. Collection uses exact domains including optional ports, bounded request/field sizes and a per-site/IP rate limit. Origin checks constrain browser integration, not a claim that public analytics is bot-proof. Proxies are not trusted through client-supplied forwarded headers.

Invitations use random 144-bit tokens, hashed for lookup and encrypted for authorized redisplay. Rotation invalidates the old URL. Public signup never reveals participant lists, history or management capability; signup fields are bounded, duplicate names rejected and rate limited. The form has a nonce-based CSP and no external script, font or QR service. `Runtime.PublicURL` must be the externally reachable Euler URL for copyable scripts and mobile invitations.

## Persistence and correctness

Tables have `statistics_` or `lottery_` prefixes; repeatable migrations leave platform tables unchanged. Public resources are joined to the owning enabled installation. All management writes and their audit event commit together. Public signup and collection use their own bounded transactions without creating a synthetic personnel identity.

The shared SQLite connection serializes draw transactions. The transaction reads the room and eligible participants, runs a partial Fisher–Yates shuffle using `crypto/rand`, inserts the immutable round and winner snapshots, advances the round counter and records its audit. All transaction operations use `tx`; query rows close before another query. A request ID plus payload hash prevents repeat draws; conflicting reuse returns 409. Successful retries return the same winners. A reset increments the generation, so an old request cannot accidentally replay in a new cycle.

Participant removal excludes that person from future draws while preserving winner snapshots. Duplicate generation skips existing normalized names. Removed names are retained, so re-adding the identical name requires a distinguishable name. Counts label winner **occurrences**, because repeat wins may be enabled. The UI obtains room ownership and all history from the database; localStorage is not an authorization or results source.

Before sending a draw, the browser persists only its request ID and exact intent in sessionStorage, namespaced by actor, tenant, project, installation and room. A lost response can therefore be retried after room navigation or reload without another draw. An in-flight response updates only its original room; confirmation clears only that room's pending intent. No winner or authorization is taken from browser storage.

## Verification

`go test -race ./internal/apps/statistics ./internal/apps/lottery` uses real SQLite files and validates migrations, cross-project denial, viewer denial, malformed origins and injected identity fields, disabled installations, public signup/token rotation, real PNG QR generation, report totals, URL privacy, transactional audit failure, concurrent nonrepeating draws, concurrent idempotent retries and cycle reset behavior.

`platform/frontend/e2e/app-cloud/activities.spec.ts` drives the actual Go/SQLite platform without mocking its API: site creation, loading the real collector/widget/helper in Chromium, report rendering, site pause; mobile signup, manual/batch creation, removal, animated result, persisted history, idempotent retry and room reset. A separate fault-injection test commits a real draw, drops only its browser response, switches rooms and reloads, then verifies a retry returns the same draw ID and never creates another round. The shared app-cloud Playwright configuration starts the OIDC test issuer and real platform. Passing status is recorded by the release's actual test run, not inferred from this checklist.
