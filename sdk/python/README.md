# cloudsdk — Euler Python SDK (M-5.2)

The Python entry point for calling Euler product APIs from off-platform code,
the way the published `cloudsdk-{product}-python` packages wrap.

## Single signing contract (03§9.4 rule ⑤)

This SDK does **not** reimplement signing. It computes the CPS1-HMAC-SHA256
signature (07-security.md §4.1, adjudication D4) and is pinned to the shared
golden-vector fixture so the result matches `pkg-go/cps1` byte-for-byte. The
Go `cps1` package, this Python module, and the gateway verifier share one
definition — drift is caught by regression, not by customers.

Errors parse into the unified model (03§9.3): a non-2xx with a `{Code, Message}`
body surfaces an `ApiError` carrying the business code and HTTP status.

## Usage

```python
from cloudsdk import Client, Config, ApiRequest

client = Client(Config(ak="EU...", sk="...", region="cn-north-1"))
resp = client.call(ApiRequest(
    product_code="euecs", method="POST", path="/",
    query={"Action": "RunInstances", "Version": "2026-08-01"},
    body=b'{"ImageId":"img-001","InstanceType":"s2.large"}',
))
print(resp.status_code, resp.body)
```

## Tests

The golden-vector regression reads `proto-hub/testdata/cps1-golden-vectors.json`
and asserts every vector's `expected_signature` / `expected_authorization` is
reproduced exactly:

```sh
cd sdk/python
python -m pytest            # 12 tests, incl. 7 golden vectors
```
