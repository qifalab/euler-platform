# CPS1-HMAC-SHA256 — Implementation Notes

The signing algorithm is the platform's **wire contract** (07-security.md §4.1,
adjudication D4). It ships with the SDK and never takes a breaking change.

## Single source of truth

| Artifact | Role |
|---|---|
| `pkg-go/cps1` | Authoritative Go implementation; carries the algorithm tests |
| `proto-hub/testdata/cps1-golden-vectors.json` | Shared cross-language fixture |
| `services/svc-iam/cmd/gen-vectors` | Regenerates the fixture from the Go implementation |

Every implementation — the gateway verifier, each published SDK, the OpenAPI
Explorer, and the doc-site examples — must reproduce `expected_signature` in
the fixture exactly. 03§9.4 rule ⑤ requires all of them to share one definition
specifically to prevent "SDK / 文档 / 网关 三处算法定义漂移".

Regenerate the fixture:

```sh
cd services/svc-iam
go run ./cmd/gen-vectors > ../../proto-hub/testdata/cps1-golden-vectors.json
```

**A change to the fixture output is a change to the wire contract.** Treat it as
a breaking API change requiring a new SDK major version, not a routine update.

## Percent-encoding: strict RFC 3986

Both path segments and query components use strict RFC 3986 encoding — only the
unreserved set `[A-Za-z0-9-._~]` passes through unencoded; everything else
becomes `%XX` with uppercase hex.

This is a deliberate choice, not an accident of implementation. Go's
`url.PathEscape` leaves `$ @ : = & +` unescaped, while a straightforward
canonicaliser in another SDK language escapes them. Using the language-default
helper on each side produces **different canonical requests for the same URL**,
so a non-Go SDK caller
would receive 403 from a Go-verified gateway for any path containing those
characters — a bug that only appears in production, only for some paths, and
only for some SDK languages.

The golden vectors `path-reserved-chars` and `query-reserved-chars` pin this
behaviour, and `TestCanonicalURIStrictRFC3986` / `TestQueryEncodingStrictRFC3986`
guard it independently of the fixture file.

When adding a new SDK language: do not reach for the platform's built-in URL
escaper. Implement the unreserved-set rule directly and validate against the
fixture.

## Verification order (fixed by 07§4.1)

The order is load-bearing — each step is cheaper than the next, so failing fast
also sheds load under attack:

1. Time window — `|now − x-cps-date| ≤ 15 min`
2. AK existence & status — enabled, not deleted, owner account usable
3. STS token validity — when `x-cps-security-token` is present
4. Signature recomputation — constant-time compare
5. Nonce dedup — Redis `SET NX`, TTL 16 min
6. Identity injection — `X-Sc-Account-Id` / `X-Sc-Identity` / `X-Sc-TraceId`

Nonce dedup deliberately follows signature verification: an attacker must forge
a valid signature before they can consume nonce-store capacity.

If the nonce store is unavailable, verification **fails closed** — an
unreachable replay store means the request cannot be proven fresh.

## Scope binding

`region` and `service` are resolved by the gateway from the routed product
subdomain (`{productCode}.api.starcloud.cn`), never from client input. They
participate in the derived signing key, so a signature scoped to one service
cannot be replayed against another. `TestVerifyRejectsWrongScope` covers this.

## SK storage is reversible by necessity

Verification recomputes HMAC with the real SK, so SK cannot be hashed. It is
stored as a KMS envelope ciphertext (`sk_cipher` + `sk_key_version`) and
decrypted into a short-lived gateway cache keyed by ciphertext+version, so key
rotation invalidates the cache naturally (adjudication S5, overturning the
`sk_hash` design in 03§6.1).
