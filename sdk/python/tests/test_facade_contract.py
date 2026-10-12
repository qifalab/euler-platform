"""Client.invoke against a local fake of the OpenAPI facade (services/svc-openapi).

The fake speaks the facade's contract: Action/Version from the query only,
parameters from a JSON object body (recognised by its leading ``{``) or a form
body, the success envelope ``{RequestId, Code: "OK", Data}`` and the error
envelope ``{RequestId, Code, Message}`` with the facade's codes. Creates are
idempotent on ClientToken, as svc-order makes them.
"""

from __future__ import annotations

import hashlib
import http.server
import json
import threading
import urllib.parse

import pytest

from cloudsdk import (
    API_VERSION,
    ApiError,
    AuthorizationError,
    Client,
    Config,
    IdempotentParameterMismatchError,
    IncorrectStatusError,
    InsufficientBalanceError,
    InvalidParameterError,
    QuotaExceededError,
    ResourceNotFoundError,
    ServerError,
    ThrottlingError,
)


class FakeFacade:
    def __init__(self) -> None:
        self.requests: list[dict] = []
        self.tokens: dict[str, str] = {}
        # Answers to give before serving normally: (status, envelope) pairs.
        self.scripted: list[tuple[int, dict]] = []
        self.drop_creates = 0
        self.seq = 0

    def parse(self, handler) -> tuple[str, dict, dict]:
        length = int(handler.headers.get("Content-Length") or 0)
        body = handler.rfile.read(length) if length else b""
        url = urllib.parse.urlsplit(handler.path)
        query = urllib.parse.parse_qs(url.query, keep_blank_values=True)
        headers = {k.lower(): v for k, v in handler.headers.items()}
        # What the gateway checks before the facade sees the call.
        assert headers["authorization"].startswith("CPS1-HMAC-SHA256 Credential=AKTEST/")
        assert headers["x-cps-content-sha256"] == hashlib.sha256(body).hexdigest()
        params: dict = {}
        stripped = body.strip()
        if stripped.startswith(b"{"):
            for k, v in json.loads(stripped).items():
                if v is not None:
                    params[k] = v
        elif stripped:
            assert headers.get("content-type", "").startswith("application/x-www-form-urlencoded")
            for k, vs in urllib.parse.parse_qs(stripped.decode(), keep_blank_values=True).items():
                params[k] = vs if len(vs) > 1 else vs[0]
        action = query.get("Action", [""])[0]
        self.requests.append(
            {"action": action, "query": query, "params": params, "headers": headers, "body": body}
        )
        return action, params, query

    def serve(self, action: str, params: dict) -> tuple[int, dict]:
        if self.scripted:
            return self.scripted.pop(0)
        if action == "RunInstances":
            token = params.get("ClientToken", "")
            if token and token in self.tokens:
                instance_id = self.tokens[token]
            else:
                if "InstanceType" not in params:
                    return 400, {"Code": "MissingParameter",
                                 "Message": "The input parameter InstanceType that is mandatory for processing this request is not supplied."}
                self.seq += 1
                instance_id = "i-%d" % self.seq
                if token:
                    self.tokens[token] = instance_id
            if self.drop_creates:
                self.drop_creates -= 1
                return 503, {"Code": "ServiceUnavailable",
                             "Message": "The request has failed due to a temporary failure of the server. Retry the request later."}
            return 200, {"Code": "OK", "Data": {"InstanceId": instance_id, "OrderId": "77", "Status": "Creating"}}
        if action == "DescribeInstances":
            ids = params.get("InstanceIds", [])
            ids = ids if isinstance(ids, list) else [ids]
            return 200, {"Code": "OK", "Data": {
                "Instances": [{"InstanceId": i, "Status": "Running"} for i in ids],
                "TotalCount": len(ids), "PageNumber": 1, "PageSize": 10}}
        if action == "DeleteVpc":
            return 200, {"Code": "OK", "Data": None}
        return 400, {"Code": "InvalidAction.NotFound",
                     "Message": "The specified action %r is not supported by euecs." % action}


@pytest.fixture()
def facade():
    fake = FakeFacade()

    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def do_POST(self):  # noqa: N802
            action, params, query = fake.parse(self)
            assert query.get("Version") == [API_VERSION]
            status, env = fake.serve(action, params)
            env = dict(env)
            env.setdefault("RequestId", "req-%d" % len(fake.requests))
            raw = json.dumps(env).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, *args):
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    client = Client(Config(ak="AKTEST", sk="sktest", region="cn-north-1",
                           endpoint="http://127.0.0.1:%d" % server.server_address[1],
                           retry_backoff=0.001))
    try:
        yield fake, client
    finally:
        server.shutdown()


def test_invoke_sends_action_in_query_json_body_and_returns_data(facade):
    fake, client = facade
    data = client.invoke("euecs", "RunInstances", {"InstanceType": "s2.large", "ImageId": "img-001", "Period": 3})
    assert data == {"InstanceId": "i-1", "OrderId": "77", "Status": "Creating"}
    req = fake.requests[0]
    assert req["query"]["Action"] == ["RunInstances"]
    assert "Action" not in req["params"]  # the body never carries the Action
    assert req["headers"]["content-type"] == "application/json"
    assert req["body"].startswith(b"{")
    assert req["params"]["InstanceType"] == "s2.large"
    assert req["params"]["Period"] == 3


def test_create_gets_a_client_token_and_retries_reuse_it(facade):
    fake, client = facade
    fake.drop_creates = 2  # the order is made, the answer is lost twice
    data = client.invoke("euecs", "RunInstances", {"InstanceType": "s2.large"})
    assert data["InstanceId"] == "i-1"
    tokens = [r["params"]["ClientToken"] for r in fake.requests]
    assert len(tokens) == 3 and len(set(tokens)) == 1
    assert 0 < len(tokens[0]) <= 64
    assert fake.seq == 1  # one instance, not three
    # The next create is a new purchase with a new token.
    client.invoke("euecs", "RunInstances", {"InstanceType": "s2.large"})
    assert fake.requests[-1]["params"]["ClientToken"] != tokens[0]
    assert fake.seq == 2


def test_explicit_client_token_is_passed_through(facade):
    fake, client = facade
    first = client.invoke("euecs", "RunInstances", {"InstanceType": "s2.large"}, client_token="order-2026-10-09")
    again = client.invoke("euecs", "RunInstances", {"InstanceType": "s2.large", "ClientToken": "order-2026-10-09"})
    assert first["InstanceId"] == again["InstanceId"] == "i-1"
    assert [r["params"]["ClientToken"] for r in fake.requests] == ["order-2026-10-09"] * 2


def test_non_create_actions_get_no_client_token(facade):
    fake, client = facade
    data = client.invoke("euecs", "DescribeInstances", {"InstanceIds": ["i-1", "i-2"], "RegionId": None})
    assert [i["InstanceId"] for i in data["Instances"]] == ["i-1", "i-2"]
    assert "ClientToken" not in fake.requests[0]["params"]
    assert "RegionId" not in fake.requests[0]["params"]  # None = not supplied


def test_form_body_sends_lists_as_repeated_keys(facade):
    fake, client = facade
    data = client.invoke("euecs", "DescribeInstances", {"InstanceIds": ["i-1", "i-2"], "DryRun": False}, form=True)
    assert data["TotalCount"] == 2
    req = fake.requests[0]
    assert req["headers"]["content-type"] == "application/x-www-form-urlencoded"
    assert req["params"] == {"InstanceIds": ["i-1", "i-2"], "DryRun": "false"}


def test_data_null_is_an_empty_object(facade):
    _, client = facade
    assert client.invoke("euvpc", "DeleteVpc", {"VpcId": "vpc-1"}) == {}


@pytest.mark.parametrize(
    "status,code,cls",
    [
        (404, "InvalidInstanceId.NotFound", ResourceNotFoundError),
        (409, "IncorrectInstanceStatus", IncorrectStatusError),
        (409, "ConcurrentModification", IncorrectStatusError),
        (400, "MissingParameter", InvalidParameterError),
        (400, "InvalidInstanceType.NotFound", InvalidParameterError),
        (400, "InvalidCidrBlock", InvalidParameterError),
        (400, "InvalidAction.NotFound", InvalidParameterError),
        (409, "IdempotentParameterMismatch", IdempotentParameterMismatchError),
        (403, "QuotaExceeded", QuotaExceededError),
        (403, "InsufficientBalance", InsufficientBalanceError),
        (403, "IAM.SignatureDoesNotMatch", AuthorizationError),
        (429, "Common.Throttling", ThrottlingError),
        (503, "ServiceUnavailable", ServerError),
        (409, "BucketAlreadyExists", ApiError),
    ],
)
def test_error_envelope_raises_typed_error(facade, status, code, cls):
    fake, client = facade
    client.config.max_retries = 0
    fake.scripted = [(status, {"RequestId": "req-err", "Code": code, "Message": "it failed"})]
    with pytest.raises(cls) as ei:
        client.invoke("euecs", "StartInstance", {"InstanceId": "i-9"})
    err = ei.value
    assert isinstance(err, ApiError)
    assert (err.code, err.message, err.request_id, err.http_status) == (code, "it failed", "req-err", status)
    assert "req-err" in str(err)


def test_unknown_action_is_the_facades_invalid_action(facade):
    _, client = facade
    with pytest.raises(InvalidParameterError) as ei:
        client.invoke("euecs", "LaunchRocket")
    assert ei.value.code == "InvalidAction.NotFound"
    assert ei.value.request_id


def test_2xx_with_error_code_or_without_envelope_is_an_error(facade):
    fake, client = facade
    fake.scripted = [(200, {"RequestId": "r-1", "Code": "InvalidParameter", "Message": "bad"})]
    with pytest.raises(InvalidParameterError) as ei:
        client.invoke("euecs", "DescribeInstances")
    assert ei.value.request_id == "r-1"
    fake.scripted = [(200, {"Code": "OK", "Data": [1, 2]})]
    with pytest.raises(ApiError):
        client.invoke("euecs", "DescribeInstances")
