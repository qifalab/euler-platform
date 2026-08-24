"""Client behaviour tests: redirect refusal, retry/backoff, encoding, creds.

These complement the golden-vector regression: they exercise the HTTP client
layer against a real local ``http.server`` so the on-the-wire URL, redirect
handling, and retry policy are tested end-to-end (no urllib monkeypatching).
"""

from __future__ import annotations

import http.server
import os
import threading
from typing import Callable

import pytest

import cloudsdk
from cloudsdk import (
    ApiError,
    ApiRequest,
    Client,
    Config,
    Credentials,
    _parse_error,
    _resolve_credentials,
    _retry_sleep_seconds,
    sign,
)


class _Recorder:
    """Shared state between the test and the handler."""

    def __init__(self, script: Callable[[int, http.server.BaseHTTPRequestHandler], None]) -> None:
        self.requests: list[dict] = []
        self.script = script


def _make_server(recorder: _Recorder):
    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def _handle(self) -> None:
            n = len(recorder.requests)
            recorder.requests.append(
                {
                    "raw_path": self.path,
                    "method": self.command,
                    "headers": {k.lower(): v for k, v in self.headers.items()},
                }
            )
            recorder.script(n, self)

        do_GET = _handle
        do_POST = _handle

        def log_message(self, *args):  # silence
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server


def _send(handler, status: int, body: bytes = b"{}", headers: dict | None = None) -> None:
    handler.send_response(status)
    for k, v in (headers or {}).items():
        handler.send_header(k, v)
    handler.send_header("Content-Length", str(len(body)))
    handler.end_headers()
    handler.wfile.write(body)


def _client(server, **cfg) -> Client:
    port = server.server_address[1]
    endpoint = cfg.pop("endpoint_path", "")
    return Client(
        Config(
            ak="AKTEST",
            sk="sktest",
            region="cn-north-1",
            endpoint="http://127.0.0.1:%d%s" % (port, endpoint),
            retry_backoff=0.01,
            **cfg,
        )
    )


# --- redirect refusal ---------------------------------------------------------


def test_redirects_are_not_followed_and_surface_as_errors():
    def script(n, handler):
        _send(handler, 302, b"", {"Location": "http://evil.example.com/steal"})

    rec = _Recorder(script)
    server = _make_server(rec)
    try:
        client = _client(server)
        with pytest.raises(ApiError) as ei:
            client.call(ApiRequest(product_code="scecs"))
        assert ei.value.http_status == 302
        # Exactly one request: the redirect target was never fetched (which
        # would have replayed the Authorization header off-host).
        assert len(rec.requests) == 1
    finally:
        server.shutdown()


# --- retry / Retry-After ------------------------------------------------------


def test_retries_on_429_respecting_retry_after_then_succeeds():
    def script(n, handler):
        if n < 2:
            _send(handler, 429, b'{"Code":"Common.Throttled","Message":"slow down"}',
                  {"Retry-After": "0"})
        else:
            _send(handler, 200, b'{"ok":true}')

    rec = _Recorder(script)
    server = _make_server(rec)
    try:
        client = _client(server, max_retries=3)
        resp = client.call(ApiRequest(product_code="scecs"))
        assert resp.status_code == 200
        assert resp.body == b'{"ok":true}'
        assert len(rec.requests) == 3
        # Each attempt is a fresh signed request: distinct nonces.
        nonces = {r["headers"]["x-cps-nonce"] for r in rec.requests}
        assert len(nonces) == 3
    finally:
        server.shutdown()


def test_retry_exhaustion_on_5xx_raises_last_error_with_request_id():
    def script(n, handler):
        _send(handler, 503, b'{"Code":"Common.Unavailable","Message":"down"}',
              {"x-cps-request-id": "req-abc", "Retry-After": "0"})

    rec = _Recorder(script)
    server = _make_server(rec)
    try:
        client = _client(server, max_retries=2)
        with pytest.raises(ApiError) as ei:
            client.call(ApiRequest(product_code="scecs"))
        assert ei.value.code == "Common.Unavailable"
        assert ei.value.http_status == 503
        assert ei.value.request_id == "req-abc"
        assert len(rec.requests) == 3  # 1 initial + 2 retries
    finally:
        server.shutdown()


def test_4xx_other_than_429_is_not_retried():
    def script(n, handler):
        _send(handler, 403, b'{"RequestId":"r-1","Code":"Iam.Denied","Message":"no"}')

    rec = _Recorder(script)
    server = _make_server(rec)
    try:
        client = _client(server, max_retries=3)
        with pytest.raises(ApiError) as ei:
            client.call(ApiRequest(product_code="scecs"))
        assert ei.value.code == "Iam.Denied"
        assert ei.value.request_id == "r-1"  # body RequestId preserved
        assert len(rec.requests) == 1
    finally:
        server.shutdown()


def test_retry_sleep_prefers_retry_after_and_caps():
    cfg = Config(ak="a", sk="s", region="r", retry_backoff=0.5, retry_max_sleep=3.0)
    assert _retry_sleep_seconds(0, "2", cfg) == 2.0
    assert _retry_sleep_seconds(0, "999", cfg) == 3.0  # capped
    assert _retry_sleep_seconds(1, None, cfg) == 1.0  # 0.5 * 2**1
    assert _retry_sleep_seconds(0, "not-a-number", cfg) == 0.5  # fallback


# --- wire encoding matches the signed canonical form --------------------------


def test_url_path_and_query_use_rfc3986_encoding_matching_signature():
    def script(n, handler):
        _send(handler, 200, b"{}")

    rec = _Recorder(script)
    server = _make_server(rec)
    try:
        client = _client(server)
        client.call(
            ApiRequest(
                product_code="scecs",
                path="/a b/c",
                query={"Action": "Run Instances", "T~": "x+y"},
            )
        )
        raw = rec.requests[0]["raw_path"]
        # Spaces are %20 (never "+"), unreserved "~" survives, "+" is %2B.
        assert raw == "/a%20b/c?Action=Run%20Instances&T~=x%2By"
    finally:
        server.shutdown()


def test_endpoint_path_prefix_is_preserved_and_signed():
    def script(n, handler):
        _send(handler, 200, b"{}")

    rec = _Recorder(script)
    server = _make_server(rec)
    try:
        client = _client(server, endpoint_path="/api/v1")
        client.call(ApiRequest(product_code="scecs", path="/instances"))
        assert rec.requests[0]["raw_path"] == "/api/v1/instances"
        # Root path: prefix alone.
        client.call(ApiRequest(product_code="scecs", path="/"))
        assert rec.requests[1]["raw_path"] == "/api/v1"
    finally:
        server.shutdown()


# --- signing details ----------------------------------------------------------


def test_sign_force_overwrites_x_cps_date():
    stale = "20200101T000000Z"
    fresh = "20260804T093000Z"
    signed = sign(
        method="GET",
        host="scecs.api.starcloud.cn",
        path="/",
        query={},
        headers={
            "host": "scecs.api.starcloud.cn",
            "x-cps-date": stale,
            "x-cps-nonce": "n",
        },
        body=b"",
        creds=Credentials(ak="AK", sk="SK"),
        region="cn-north-1",
        service="ecs",
        date_value=fresh,
    )
    assert signed.headers["x-cps-date"] == fresh


# --- credentials --------------------------------------------------------------


def test_env_fallback_for_credentials(monkeypatch):
    monkeypatch.setenv("SC_ACCESS_KEY_ID", "AKENV")
    monkeypatch.setenv("SC_SECRET_ACCESS_KEY", "SKENV")
    creds = _resolve_credentials(Config(ak="", sk="", region="r"))
    assert creds.ak == "AKENV"
    assert creds.sk == "SKENV"
    # Explicit config wins over env.
    creds = _resolve_credentials(Config(ak="AKCFG", sk="SKCFG", region="r"))
    assert creds.ak == "AKCFG"
    assert creds.sk == "SKCFG"


def test_credentials_repr_does_not_leak_secret():
    creds = Credentials(ak="AKVISIBLE", sk="SUPERSECRET", security_token="TOKSECRET")
    r = repr(creds)
    assert "AKVISIBLE" in r
    assert "SUPERSECRET" not in r
    assert "TOKSECRET" not in r


# --- exports ------------------------------------------------------------------


def test_all_exports_credentials_and_signed_request():
    assert "Credentials" in cloudsdk.__all__
    assert "SignedRequest" in cloudsdk.__all__


def test_py_typed_marker_present():
    pkg_dir = os.path.dirname(cloudsdk.__file__)
    assert os.path.exists(os.path.join(pkg_dir, "py.typed"))
