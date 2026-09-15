# APISIX Gateway — Implementation Notes

Source of truth: `04-middleware-infrastructure.md` §3 (zones, routes, discovery,
deployment), `07-security.md` §4/§5 (plugin chain, signature verification).

## Why the signature algorithm is not implemented in Lua

`eu-auth` bypasses to svc-iam's `/internal/openapi/verify` endpoint rather than
recomputing CPS1-HMAC-SHA256 in the plugin. APISIX ships no cloud-vendor
signature plugin, so writing one was an option — and the wrong one.

The algorithm is the platform's wire contract (07§4.1, adjudication D4). It
already has implementations to keep in step: `pkg-go/cps1` and
each published SDK. A fourth in Lua would be a fourth place
to drift, and 03§9.4 rule ⑤ exists specifically to prevent
"SDK / 文档 / 网关 三处算法定义漂移".

The cost is one extra hop per request. The endpoint budget is P99 < 10 ms
(local signature computation plus cached AK metadata, no synchronous database
access), which 04§3.4 accepts explicitly.

## Plugin chain

```
ip-restriction → limit-req → eu-auth → eu-authorize → eu-audit → upstream
     (blacklist)  (IP floor)  (authn)    (authz)      (side-stream)
```

Order is load-bearing, not cosmetic: each stage is cheaper than the next, so
failing fast also sheds load under attack. APISIX runs plugins by descending
priority, so the custom plugins declare 2500 / 2400 / 2300 to sit in this order
after the built-ins.

| Plugin | Phase | Responsibility |
|---|---|---|
| `eu-auth` | `rewrite` | CPS1 signature (OpenAPI zone) or JWT (console zone); injects identity headers |
| `eu-authorize` | `access` | Action-level authorization via svc-iam `CheckAccess`; L1 cache 30 s |
| `eu-audit` | `log` | Emits the audit event to Kafka after the response; never blocks the request |

## Identity header spoofing

`eu-auth` strips **every** client-supplied `X-Euler-*` header before injecting its
own. Without that step, a caller could simply present `X-Euler-Account-Id: <victim>`
and any upstream that trusts the header would serve another tenant's data.

Upstream services accept these headers only from gateway mTLS or the internal
CIDR; anything else presenting them is rejected 403 (07§4.3). The strip is the
gateway half of that contract.

## Scope binding

`region` and `service` are configured **on the route**, never read from the
request. They feed the derived signing key, so binding them to the route is what
stops a signature scoped to `euecs` being replayed against `euoss`. The
validator enforces that every signature-mode route sets both.

## Fail-closed behaviour

Three places deliberately fail closed rather than open:

- `eu-auth` when the verify endpoint is unreachable → 503. An unreachable
  verifier means the caller's identity cannot be proven.
- `eu-authorize` when `CheckAccess` fails → 503, and the failure is **not**
  cached, so a transient outage does not pin a deny for 30 seconds.
- `eu-authorize` when `eu-auth` did not run → 403. A route missing the auth
  plugin is a misconfiguration, not an anonymous request.

## Nacos discovery

Subscription strings must be `{GROUP}@@{serviceName}` with Group equal to the
service name (04§4.3). A mismatch **fails silently** — discovery returns no
upstreams and the route 502s with nothing in the logs pointing at the cause.
This is selection pit #2, and the route validator checks the form on every MR.

Nacos is pinned to 2.4.x LTS with auth enabled and a read-only `apisix_ro`
account. Verify discovery compatibility in staging before any 3.x upgrade.

## Zone split

Three independent data-plane Deployments (portal / console / OpenAPI) share one
3-node etcd. Separate clusters mean a marketing campaign on the portal cannot
exhaust workers serving paying customers' API calls — the blast radius stops at
the zone. etcd splits off with the cluster only when OpenAPI QPS exceeds 20k or
compliance demands isolation (04§3.1).

etcd is a gateway SPOF if unmanaged (selection pit #3): it is on the middleware
ops checklist for backup, monitoring, and version upgrades, with snapshots every
6 h to internal MinIO.

## Documented exemptions

| Route | Exemption | Reason |
|---|---|---|
| `console-auth` | no `eu-auth` | Mints the session; cannot require a session to obtain one. Protected by a 5/min per-IP limit plus the `X-Requested-With` CSRF floor. |
| `openapi-euoss-data` | no `eu-auth` | S3-compatible data plane uses MinIO's own SigV4 for ecosystem compatibility (04§9.4). The single documented exemption from the platform signature contract. |

Both are listed in `tools/check-apisix-routes.py`; adding a new exemption means
editing that list, which forces the reason into review.

## Validation without a cluster

Neither Docker nor a Lua interpreter is available on the current build host, so
these manifests are **source-only and unexecuted**. What has been verified:

- `tools/check-yaml-syntax.py` — every manifest parses (Helm templates via
  directive substitution)
- `tools/check-lua-plugins.py` — balanced blocks and delimiters, APISIX module
  contract (`check_schema`, `priority`, `return _M`)
- `tools/check-apisix-routes.py` — auth coverage, chain order, scope binding,
  Nacos subscription form, product codes against the phase-1 set

The route validator was verified against a fixture of deliberately broken routes
and caught all eight injected faults.

**Still unverified, and only a real cluster can settle it:** that the plugins
load into APISIX, that the `resty.*` APIs are used correctly, and that the
forward-auth round trip meets its 10 ms budget. `platform/ci-templates/manifest-repo-pipeline.yml`
adds `luacheck` and `kubeconform` for when the container toolchain is available.
