# Product connectors

This package implements HTTP adapters over the public projects' existing APIs.
It does not provision infrastructure, issue upstream service tokens, exchange
Euler sessions for product sessions, or grant Euler roles from verification data.

## Verified upstream contracts

Reviewed on 2026-09-24. API paths are fixed in code; tenants cannot provide proxy
paths. The deployment config controls allowed origins and identity sources.

| Product | Operations used | Credential / response boundary |
| --- | --- | --- |
| [WeAuth](https://github.com/ctipscn/weauth/blob/main/backend/main.go) | `GET /api/auth/me`; `GET/POST /api/admin/sites`; `DELETE /api/admin/sites/{id}` | User Bearer JWT; authenticated `id` binds the account. List returns an array. Create takes `{name,domains}` and returns a site plus secret; Euler discards the secret. |
| [Database](https://github.com/ctipscn/ecloud-database/blob/main/ecloud-edb-backend/routers/databases.py) | `GET /api/users/me`; `GET /api/databases` | User Bearer JWT; response `{databases,total}`. Euler projects only ID/name/type/status, never `connection_info` containing passwords. |
| [Storage](https://github.com/ctipscn/ecloud-storage/blob/main/ecloud-oss-backend/routers/s3api.py) | `GET /api/s3/user/quota`; `GET /api/s3/buckets` | `X-Access-Key` + `X-Secret-Key`; verified `user_id` binds the account. These upstream keys are account-wide, while this adapter only exposes reads. Quota is the upstream basic account quota, not an inferred bill or package allowance. |
| [EID](https://github.com/miaojilab/emoera-eid/blob/main/members/views.py) | `GET /api/latest-verification/?oauth_id=…` | Uses only the authenticated Euler subject from the configured provider. Returns the latest approved record; absence of expiry/revocation semantics is displayed explicitly. `identity_type` is a string such as `active` or `core`. |
| [Trust](https://github.com/miaojilab/trust-center/blob/main/controllers/kyc.controller.js) | `GET /api/verification/status?oauthId=…&schemeId=…` | Subject comes from the verified session, scheme from deployment config. Only `{success,data:{verified,status}}` is decoded; no details/materials endpoint is called. |
| [WitShield](https://github.com/witkitlab/witshield/blob/main/internal/httpapi/health.go) | `GET /readyz` | No credential; checks Controller readiness only. Device inventory and repair workflows stay in the original authenticated console. |
| [Statistics](https://github.com/ctipscn/ecloud-statistics), [Lottery](https://github.com/miaojilab/emoera-lottery-system) | Independent instance link | No upstream data requests or admin credentials. Summary returns `unsupported` with an explanation and the configured console link. |

## Integration rules

- The platform authorizes every project request before calling this package.
- `ValidateConnectionContext` ignores the submitted external account ID and
  queries the upstream current-user endpoint. The returned account identity is
  canonical origin plus upstream ID. The platform must enforce one project per
  product/external account, and store its connection encrypted.
- Every data operation rechecks that the credential still identifies that
  account. A credential for another account fails with `account_mismatch`.
- Connections for independent instances use `instance:<origin>` as the binding
  identity. EID and Trust instead query the current subject and do not reserve a
  shared identity service as an exclusive project account.
- Resource deletion additionally requires the platform's recorded project
  resource binding. The adapter never replaces that authorization check.
- When changing an origin, the platform must require a newly supplied credential;
  it must not silently forward the old encrypted credential to another origin.
- User Bearer input may be the raw token or JSON
  `{"kind":"user_bearer","token":"…"}`. Storage requires JSON
  `{"kind":"access_key_pair","accessKey":"…","secretKey":"…"}`.
  These are the upstream's actual credentials, not scoped Euler tokens.

## Outbound and failure handling

Origins are exact deployment allowlist entries. HTTPS is required by default.
HTTP and private-network access are separate, explicit deployment options for
local tests or private installations. Hostname resolution is checked and the
validated IP is dialed directly, with no environment proxy or redirect follow.
Requests have a deadline and a 2 MiB response limit. Error responses never include
upstream bodies, URLs, credentials or KYC materials. Incomplete/failed responses
are not interpreted as zero usage or successful verification.

`go test -race ./internal/connectors` uses real local HTTP servers with the above
contracts to check authorization headers, account changes, sensitive-field
projection, errors, deadlines, network policy and unsupported capabilities.
Those tests do not claim a production service was deployed or configured.
