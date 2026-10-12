# cloudsdk — Euler Python SDK (M-5.2)

The Python entry point for calling Euler product APIs from off-platform code,
the way the published `cloudsdk-{product}-python` packages wrap.

## Single signing contract (03§9.4 rule ⑤)

This SDK does **not** reimplement signing. It computes the CPS1-HMAC-SHA256
signature (07-security.md §4.1, adjudication D4) and is pinned to the shared
golden-vector fixture so the result matches `pkg-go/cps1` byte-for-byte. The
Go `cps1` package, this Python module, and the gateway verifier share one
definition — drift is caught by regression, not by customers.

Errors parse into the unified model (03§9.3): a non-2xx with a
`{RequestId, Code, Message}` body surfaces an `ApiError` carrying the code,
message, HTTP status and RequestId.

## Usage

`Client.invoke` speaks the OpenAPI facade's contract (services/svc-openapi):
Action and Version in the query, the parameters as a JSON object body (or a
form body with `form=True`; lists become repeated keys), and the success
envelope `{RequestId, Code: "OK", Data}` unwrapped to `Data`.

```python
from cloudsdk import Client, Config, ResourceNotFoundError

client = Client(Config(ak="EU...", sk="...", region="cn-north-1"))
data = client.invoke("euecs", "RunInstances",
                     {"InstanceType": "s2.large", "ImageId": "img-001", "ZoneId": "cn-north-1-a"})
instance_id = data["InstanceId"]

try:
    client.invoke("euecs", "StopInstance", {"InstanceId": "i-missing"})
except ResourceNotFoundError as e:
    print(e.code, e.message, e.request_id)   # InvalidInstanceId.NotFound ...
```

- **ClientToken.** A create Action (`Create*`, `Run*`, `Allocate*`) gets a
  fresh `ClientToken` unless one is passed (`client_token=` or in the
  parameters), so the SDK's automatic retries on 429/5xx (the facade answers
  a downstream outage with 503 `ServiceUnavailable`) return the resource the
  first attempt bought instead of buying another. Pass your own token to make
  a create idempotent across processes.
- **Typed errors.** Every error is an `ApiError`; the code picks the subclass:
  `InvalidParameterError` (MissingParameter, InvalidParameter, Invalid…),
  `ResourceNotFoundError` (404 …NotFound), `IncorrectStatusError`
  (Incorrect…Status, ConcurrentModification), `IdempotentParameterMismatchError`,
  `QuotaExceededError`, `InsufficientBalanceError`, `AuthorizationError`
  (401/403), `ThrottlingError` (429), `ServerError` (5xx). A 2xx that is not
  the envelope or carries a non-OK Code raises too.
- `Client.call(ApiRequest(...))` remains the raw signed round trip.

## Tests

The golden-vector regression reads `proto-hub/testdata/cps1-golden-vectors.json`
and asserts every vector's `expected_signature` / `expected_authorization` is
reproduced exactly:

```sh
cd sdk/python
python -m pytest            # golden vectors, client behaviours, facade contract
```
