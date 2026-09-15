# euler Terraform Provider (三期 M-10.2, 验收 C3)

`terraform-provider-euler` — the IaC consumer surface for the platform
(09§5.1 goal 3: "Terraform Provider、API 集成生态"; 09§5.3 C3: "Terraform
Provider 覆盖核心产品").

## Design

- **One signing implementation.** Every resource CRUD goes through
  `pkg-go/eusdk` → `pkg-go/cps1` — the same signer the gateway verifier, Go
  SDK, Python SDK and OpenAPI Explorer share (03§9.4 rule ⑤). The provider
  never re-implements CPS1; drift is caught by the golden-vector fixture
  (`proto-hub/testdata/cps1-golden-vectors.json`), not by Terraform users.
- **Secrets stay out of state.** `EULER_ACCESS_KEY` / `EULER_SECRET_KEY` are read
  from the environment; the provider config surface is endpoint + region only.
- **Resources are thin.** `EUECS/EUOSS/EUVPC/EURDS` are covered as skeletons
  sharing one `crudCall` helper; each resource is a schema + four product
  action names. A fifth product is additive, not a new code path.

## Source-only status

This module declares `terraform-plugin-framework`, which is not vendored on
this host, so the provider is **complete source + static consistency, not
built** — the same source-only convention the repo applies to Lua plugins,
K8s manifests and SQL DDL (see README "Verification status"). To build:

```sh
cd sdk/terraform
go mod download && go build ./...
```

## Covered products (C3)

| Product | Resource | Create / Read / Delete actions |
|---|---|---|
| EUECS | `euler_euecs_instance` | RunInstances / DescribeInstances / TerminateInstances |
| EUOSS | `euler_euoss_bucket` | CreateBucket / (DescribeBucket) / DeleteBucket |
| EUVPC | `euler_euvpc_vpc` | CreateVpc / — / DeleteVpc |
| EURDS | `euler_eurds_instance` | CreateInstance / — / DeleteInstance |
