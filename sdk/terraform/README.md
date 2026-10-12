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
- **Secrets stay out of state.** The access key pair is read from the
  environment, first non-empty wins in this order — the same names and order
  as the Go SDK (`pkg-go/eusdk`) and the Python SDK: `EULER_ACCESS_KEY_ID` /
  `EULER_SECRET_ACCESS_KEY`, then `EULER_ACCESS_KEY` / `EULER_SECRET_KEY`, then
  the short `EULER_AK` / `EULER_SK`. The provider config surface is endpoint +
  region only.
- **Resources are specs, not code paths.** Every resource is one
  `resourceSpec` (its attributes and the Actions that create, describe,
  modify and delete it) run by one implementation (`internal/provider`); a
  further product is a spec.

## Behaviour against the OpenAPI facade

Every Action is signed and sent to `services/svc-openapi` (via APISIX at
`{product}.api.euler.emoera.com`) as `POST /?Action=…&Version=2026-08-01` with
a JSON parameter body; answers are the platform envelope — success
`{RequestId, Code:"OK", Data}`, failure `{RequestId, Code, Message}`. Errors
are reported with their code, message and RequestId.

- **Create** sends a fresh `ClientToken` (kept in state as `client_token`).
  Transport failures, 429 and 5xx (the facade's `ServiceUnavailable`, a gateway
  timeout) are retried up to 4 times with the same token, so a retry lands on
  the resource the first attempt bought instead of buying a second one. An
  answer without the resource id is an error. The provider then polls the
  Describe action until the resource leaves `Creating` and fills every computed
  attribute; a resource that ends in `CreateFailed` stays in state with an
  error, so Terraform taints and replaces it on the next apply.
- **Read** calls the Describe/List action and reads the resource out of its
  list (`Instances[]`, `Vpcs[]`, `Disks[]`, `EipAddresses[]`, `Buckets[]` by
  exact name). A resource that is gone — an empty list or the product's
  `…NotFound` code — leaves the state (the next plan re-creates it). All
  readable attributes are refreshed; a value the API only re-spelled
  (`s2.Large` → `s2.large`, `PrePaid` → `Prepaid`) keeps the configured
  spelling.
- **Update** calls the product's modify Action for each changed attribute
  (and only then sets state); every other input forces replacement.
- **Delete** calls the delete Action (`…NotFound` counts as already deleted)
  and waits until the resource is gone.

Region: a resource without `region` (or a `zone_id`, which names its region)
is created in the provider's `region`.

## Provider configuration

```hcl
provider "euler" {
  region   = "cn-north-1"   # signing region and default resource region (default cn-north-1)
  # endpoint = "https://gw.example.com"   # optional override for every product
}
```

| Environment variable | Meaning |
|---|---|
| `EULER_ACCESS_KEY_ID` / `EULER_SECRET_ACCESS_KEY` | Access key pair (first choice) |
| `EULER_ACCESS_KEY` / `EULER_SECRET_KEY` | Same, second spelling |
| `EULER_AK` / `EULER_SK` | Same, short spelling |

Without `endpoint` each product is called at its API host
`{product}.api.euler.emoera.com`, which is how the gateway and the facade
pick the product. An `endpoint` sends every product to that one host; use it
only for a gateway that routes all products there (or a test server).

## Resources

Legend: **R** required, **O** optional, **C** computed; **new** = changing it
replaces the resource, **in place** = changed by the named Action.

Shared by the purchased products (instance, RDS, disk, EIP):
`zone_id` (O/C, new), `region` (O/C, new), `charge_type` (O/C, new;
`Prepaid`/`Postpaid`, default Postpaid), `period` (O, new; Prepaid months,
creation only), `sku_code` (C), `expired_time` (C). Shared by every resource:
`id` (C), `status` (C), `creation_time` (C), `client_token` (C), `order_id`
(C). The VPC and the bucket have `region` but no zone or purchase attributes.

| Resource | Actions (create / read / delete) | Own attributes |
|---|---|---|
| `euler_euecs_instance` | RunInstances / DescribeInstances / TerminateInstances | `instance_type` (R, in place: ModifyInstanceSpec, Postpaid only), `image_id` (R, new), `instance_name` (O/C, in place: ModifyInstanceAttribute) |
| `euler_eurds_instance` | CreateInstance / DescribeInstances / DeleteInstance | `engine_version` (R, new), `engine` (C), `instance_class` (O/C, in place: ModifyInstanceSpec, Postpaid only), `instance_name` (O/C, in place: ModifyInstanceAttribute), `instance_id` (C) |
| `euler_euvpc_vpc` | CreateVpc / DescribeVpcs / DeleteVpc | `cidr_block` (R, new), `vpc_name` (O/C, in place: ModifyVpcAttribute), `vpc_id` (C) |
| `euler_eubs_disk` | CreateDisk / DescribeDisks / DeleteDisk | `size` (R, GiB, new), `disk_category` (O/C, new), `disk_name` (O/C, in place: ModifyDiskAttribute) |
| `euler_eueip_address` | AllocateEipAddress / DescribeEipAddresses / ReleaseEipAddress | `bandwidth` (O/C, Mbps, new), `name` (O/C, new — no rename Action), `internet_charge_type` (C) |
| `euler_euoss_bucket` | CreateBucket / ListBuckets / DeleteBucket | `bucket` (R, new; also the `id`), `storage_class` (O/C, new; Standard) |

```hcl
resource "euler_euvpc_vpc" "core" {
  cidr_block = "192.168.0.0/16"
  vpc_name   = "core"
}

resource "euler_euecs_instance" "web" {
  instance_type = "s2.large"
  image_id      = "img-001"
  instance_name = "web"
  zone_id       = "cn-north-1-a"
}
```

## Limitations

- No data sources and no `terraform import` yet.
- `image_id` and `period` are not returned by the API: they are kept as
  configured and not checked for drift.
- A create that keeps failing with a retryable error after 4 attempts reports
  its ClientToken; the resource may exist by then and has to be found in the
  console (it is not adopted automatically).
- Spec changes of Prepaid resources need an upgrade order the OpenAPI does not
  offer yet (`OperationDenied.UpgradeRequiresOrder`).

## Build and test

```sh
cd sdk/terraform
go build ./... && go test ./...
```

The tests run every resource's lifecycle against an in-process fake of the
facade that verifies each request's CPS1 signature with `pkg-go/cps1`.
