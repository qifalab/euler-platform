"""Euler Python SDK (03§9.4 SDK generation).

The single Python entry point for calling Euler product APIs from
off-platform user code and automation, the way the published
``cloudsdk-{product}-python`` packages wrap it.

This module implements the CPS1-HMAC-SHA256 signing protocol (07-security.md
§4.1, adjudication D4) — the SAME contract ``pkg-go/cps1`` implements and the
gateway verifies. It is pinned to the shared golden-vector fixture
(``proto-hub/testdata/cps1-golden-vectors.json``) so a signature computed here
matches the Go implementation byte-for-byte (03§9.4 rule ⑤: SDK, docs, and
gateway share one definition; the fixture is how drift is caught across the
language boundary).

Errors are parsed into the unified error model (03§9.3): a non-2xx response with
a body shaped like ``{Code, Message}`` surfaces an :class:`ApiError` carrying
the business code and HTTP status — the same pairing the wire carries.

Usage::

    client = Client(Config(ak="EU...", sk="...", region="cn-north-1"))
    resp = client.call(ApiRequest(
        product_code="euecs", method="POST", path="/",
        query={"Action": "RunInstances", "Version": "2026-08-01"},
        body=b'{"ImageId":"img-001","InstanceType":"s2.large"}',
    ))
"""

from __future__ import annotations

import hashlib
import hmac
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from dataclasses import dataclass, field
from typing import Mapping, MutableMapping

__all__ = [
    "Config",
    "Client",
    "ApiRequest",
    "ApiResponse",
    "ApiError",
    "Credentials",
    "SignedRequest",
    "sign",
    "product_api_host",
    "service_namespace",
    "ALGORITHM",
]

# Environment fallback for credentials (mirrors the Go SDK's env chain).
ENV_ACCESS_KEY_ID = "EULER_ACCESS_KEY_ID"
ENV_SECRET_ACCESS_KEY = "EULER_SECRET_ACCESS_KEY"

# --- protocol constants (mirror pkg-go/cps1) ----------------------------------

ALGORITHM = "CPS1-HMAC-SHA256"
TERMINATOR = "cps1_request"
HEADER_PREFIX = "x-cps-"
HEADER_HOST = "host"
HEADER_DATE = "x-cps-date"
HEADER_CONTENT_SHA = "x-cps-content-sha256"
HEADER_NONCE = "x-cps-nonce"
HEADER_SECURITY_TOKEN = "x-cps-security-token"

# Default routing subdomain suffix (04-middleware §3.2).
_API_HOST_SUFFIX = ".api.euler.emoera.com"


def product_api_host(product_code: str) -> str:
    """Routing subdomain for a product: ``euecs`` -> ``euecs.api.euler.emoera.com``."""
    return product_code + _API_HOST_SUFFIX


def service_namespace(product_code: str) -> str:
    """Derive the signing service from a product code (euecs -> ecs).

    Matches the gateway verifier's derivation and the Go SDK's. Unknown products
    keep the full code as the service so a new product is signable before its
    mapping is taught here (the gateway rejects a service it does not recognise).
    """
    if product_code.startswith("eu") and len(product_code) > 2:
        return product_code[2:]
    return product_code


# --- percent-encoding: strict RFC 3986 ---------------------------------------
# Only the unreserved set [A-Za-z0-9-._~] survives; everything else becomes %XX
# with UPPERCASE hex. Keeping one routine for path segments and query components
# is what makes the canonical request reproducible across languages (the
# ``path-reserved-chars`` golden vector exists to pin this).
_UNRESERVED = set(
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
)


def _percent_encode(s: str) -> str:
    out = []
    for byte in s.encode("utf-8"):
        ch = chr(byte)
        if ch in _UNRESERVED:
            out.append(ch)
        else:
            out.append("%%%02X" % byte)
    return "".join(out)


def _canonical_uri(path: str) -> str:
    if path == "" or path == "/":
        return "/"
    if path == "*":
        return "*"
    # Encode each segment but preserve "/" separators.
    parts = path.split("/")
    return "/".join(_percent_encode(p) for p in parts)


def _canonical_query_string(query: Mapping[str, list[str] | tuple[str, ...] | str]) -> str:
    if not query:
        return ""
    # Expand to (key, value) pairs and sort lexicographically by key then value.
    pairs: list[tuple[str, str]] = []
    for k, v in query.items():
        if isinstance(v, (list, tuple)):
            for item in v:
                pairs.append((k, item))
        else:
            pairs.append((k, v))
    pairs.sort()
    return "&".join(
        "%s=%s" % (_percent_encode(k), _percent_encode(v)) for k, v in pairs
    )


def _collapse_spaces(s: str) -> str:
    out = []
    in_space = False
    for ch in s:
        if ch in " \t\n\r":
            if not in_space:
                out.append(" ")
                in_space = True
        else:
            out.append(ch)
            in_space = False
    return "".join(out)


def _canonical_headers(headers: Mapping[str, str]) -> tuple[str, str]:
    """Return (canonical_block, signed_headers_list).

    Mandatory: host, x-cps-date, x-cps-content-sha256. All x-cps-* headers
    present participate. Values are trimmed and interior whitespace collapsed.
    """
    norm: dict[str, str] = {}
    for k, v in headers.items():
        lk = k.strip().lower()
        if not lk:
            continue
        norm[lk] = _collapse_spaces(v.strip())
    for mandatory in (HEADER_HOST, HEADER_DATE, HEADER_CONTENT_SHA):
        if mandatory not in norm:
            raise ValueError("cps1: missing mandatory header %r" % mandatory)
    names = sorted(
        k for k in norm if k == HEADER_HOST or k.startswith(HEADER_PREFIX)
    )
    canonical = "".join("%s:%s\n" % (n, norm[n]) for n in names)
    signed_list = ";".join(names)
    return canonical, signed_list


def _hex_sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _hmac_sha256(key: bytes, data: bytes) -> bytes:
    return hmac.new(key, data, hashlib.sha256).digest()


def _derive_signing_key(sk: str, date: str, region: str, service: str) -> bytes:
    k_date = _hmac_sha256(("CPS1" + sk).encode("utf-8"), date.encode("utf-8"))
    k_region = _hmac_sha256(k_date, region.encode("utf-8"))
    k_service = _hmac_sha256(k_region, service.encode("utf-8"))
    return _hmac_sha256(k_service, TERMINATOR.encode("utf-8"))


def _scope(date: str, region: str, service: str) -> str:
    return "%s/%s/%s/%s" % (date, region, service, TERMINATOR)


def _canonical_request(
    method: str,
    path: str,
    query: Mapping[str, object],
    headers: MutableMapping[str, str],
) -> tuple[str, str]:
    canonical, signed_list = _canonical_headers(headers)
    uri = _canonical_uri(path)
    cqs = _canonical_query_string(query)  # type: ignore[arg-type]
    body_hash = headers.get(HEADER_CONTENT_SHA, "")
    if body_hash == "":
        raise ValueError("cps1: x-cps-content-sha256 must be set before signing")
    cr = "\n".join(
        [method.upper(), uri, cqs, canonical, signed_list, body_hash]
    )
    return cr, signed_list


def _string_to_sign(
    method: str,
    path: str,
    query: Mapping[str, object],
    headers: MutableMapping[str, str],
    date: str,
    region: str,
    service: str,
) -> str:
    cr, _ = _canonical_request(method, path, query, headers)
    date_day = date[:8]  # YYYYMMDD for the credential scope (Go: date.Format("20060102"))
    return "\n".join(
        [ALGORITHM, date, _scope(date_day, region, service), _hex_sha256(cr.encode("utf-8"))]
    )


@dataclass
class SignedRequest:
    """The output of :func:`sign`: the Authorization header + headers to attach."""

    authorization: str
    headers: dict[str, str]
    signature: str
    signed_headers: str


@dataclass
class Credentials:
    ak: str
    sk: str = field(repr=False)
    security_token: str = field(default="", repr=False)


def sign(
    method: str,
    host: str,
    path: str,
    query: Mapping[str, object],
    headers: MutableMapping[str, str],
    body: bytes,
    creds: Credentials,
    region: str,
    service: str,
    date_value: str,
) -> SignedRequest:
    """Compute the CPS1 signature and produce the Authorization header.

    This is the single signing routine the SDK, golden-vector fixture, and (via
    the Go ``cps1`` package) the gateway share. ``date_value`` is the
    ``x-cps-date`` header value in ``YYYYMMDDTHHMMSSZ`` form and must already be
    set in ``headers`` (matching the Go contract).

    The caller must have set host, x-cps-date, x-cps-content-sha256, and
    x-cps-nonce in ``headers``. If x-cps-content-sha256 is empty it is computed
    from ``body`` here (the empty-body case hashes the empty string).
    """
    if not creds.ak or not creds.sk:
        raise ValueError("cps1: credentials required")
    if not region or not service:
        raise ValueError("cps1: region and service required")
    hdrs = dict(headers)
    if HEADER_HOST not in hdrs:
        hdrs[HEADER_HOST] = host
    # Force-overwrite x-cps-date with the value being signed (Go parity):
    # a stale caller-supplied date must never win over date_value, or the
    # signature and the header the wire carries would disagree.
    hdrs[HEADER_DATE] = date_value
    if not hdrs.get(HEADER_CONTENT_SHA):
        hdrs[HEADER_CONTENT_SHA] = _hex_sha256(body)
    if HEADER_NONCE not in hdrs:
        raise ValueError("cps1: x-cps-nonce required")
    if creds.security_token:
        hdrs[HEADER_SECURITY_TOKEN] = creds.security_token

    date_day = date_value[:8]  # YYYYMMDD
    sts = _string_to_sign(method, path, query, hdrs, date_value, region, service)
    k_signing = _derive_signing_key(creds.sk, date_day, region, service)
    signature = _hmac_sha256(k_signing, sts.encode("utf-8")).hex()
    _, signed_list = _canonical_headers(hdrs)
    authorization = (
        "%s Credential=%s/%s, SignedHeaders=%s, Signature=%s"
        % (ALGORITHM, creds.ak, _scope(date_day, region, service), signed_list, signature)
    )
    return SignedRequest(
        authorization=authorization,
        headers=hdrs,
        signature=signature,
        signed_headers=signed_list,
    )


# --- HTTP client (stdlib only, mirrors the Go SDK) ---------------------------


@dataclass
class Config:
    """SDK credential + transport configuration."""

    ak: str
    sk: str = field(repr=False)
    region: str = ""
    service: str = ""  # derived from product_code when empty (euecs->ecs)
    security_token: str = field(default="", repr=False)
    endpoint: str = ""  # overrides the routing subdomain (may carry a path prefix)
    timeout: float = 30.0
    max_retries: int = 2  # extra attempts on 429/5xx (0 disables retry)
    retry_backoff: float = 0.5  # base seconds for exponential backoff
    retry_max_sleep: float = 30.0  # cap for any single sleep (incl. Retry-After)


@dataclass
class ApiRequest:
    product_code: str
    method: str = "GET"
    path: str = "/"
    query: Mapping[str, str | list[str]] = field(default_factory=dict)
    body: bytes = b""
    extra_headers: Mapping[str, str] = field(default_factory=dict)


@dataclass
class ApiResponse:
    status_code: int
    headers: dict[str, str]
    body: bytes


class ApiError(Exception):
    """Unified platform error (03§9.3). Carries the business code + HTTP status."""

    def __init__(
        self, code: str, http_status: int, message: str, request_id: str = ""
    ) -> None:
        super().__init__("%s: %s" % (code, message))
        self.code = code
        self.http_status = http_status
        self.message = message
        self.request_id = request_id


class _NoRedirectHandler(urllib.request.HTTPRedirectHandler):
    """Refuse to follow redirects.

    Following a redirect would replay the signed Authorization header (and the
    body) at an attacker-chosen location; the Go SDK likewise treats 3xx as a
    terminal response. Returning None makes urllib raise HTTPError for the 3xx.
    """

    def redirect_request(self, req, fp, code, msg, headers, newurl):  # noqa: D102
        return None


_OPENER = urllib.request.build_opener(_NoRedirectHandler())


def _open_request(request: urllib.request.Request, timeout: float):
    """Single seam for the HTTP round-trip (tests monkeypatch this)."""
    return _OPENER.open(request, timeout=timeout)  # noqa: S310


def _resolve_credentials(config: Config) -> Credentials:
    """Config credentials, falling back to EULER_ACCESS_KEY_ID/EULER_SECRET_ACCESS_KEY."""
    ak = config.ak or os.environ.get(ENV_ACCESS_KEY_ID, "")
    sk = config.sk or os.environ.get(ENV_SECRET_ACCESS_KEY, "")
    return Credentials(ak=ak, sk=sk, security_token=config.security_token)


def _retry_sleep_seconds(attempt: int, retry_after: str | None, config: Config) -> float:
    """Exponential backoff, overridden by a parseable Retry-After header."""
    if retry_after:
        try:
            return min(max(float(retry_after.strip()), 0.0), config.retry_max_sleep)
        except ValueError:
            pass  # HTTP-date form or garbage: fall back to backoff
    return min(config.retry_backoff * (2 ** attempt), config.retry_max_sleep)


class Client:
    """SDK handle. Construct once; safe to share (stateless beyond config)."""

    def __init__(self, config: Config) -> None:
        self.config = config

    def _host_and_prefix(self, product_code: str) -> tuple[str, str]:
        """Resolve (host, path_prefix) from the endpoint override.

        An endpoint like ``https://gw.example.com/api/v1`` keeps ``/api/v1`` as
        a prefix prepended to every request path (it is signed and sent; it is
        NOT silently discarded).
        """
        if self.config.endpoint:
            h = self.config.endpoint
            h = h.removeprefix("https://").removeprefix("http://")
            prefix = ""
            if "/" in h:
                h, rest = h.split("/", 1)
                rest = rest.rstrip("/")
                if rest:
                    prefix = "/" + rest
            return h, prefix
        return product_api_host(product_code), ""

    def _host_for(self, product_code: str) -> str:
        return self._host_and_prefix(product_code)[0]

    def call(self, req: ApiRequest) -> ApiResponse:
        if not req.product_code:
            raise ValueError("eusdk: product_code is required")
        creds = _resolve_credentials(self.config)
        host, path_prefix = self._host_and_prefix(req.product_code)
        service = self.config.service or service_namespace(req.product_code)
        method = (req.method or "GET").upper()
        path = req.path or "/"
        if path_prefix:
            path = path_prefix + ("" if path == "/" else path)
        query = dict(req.query)

        # Normalize query to the list-valued form the canonicaliser expects.
        normalized_query: dict[str, list[str]] = {}
        for k, v in query.items():
            if isinstance(v, (list, tuple)):
                normalized_query[k] = list(v)
            else:
                normalized_query[k] = [str(v)]

        body = req.body or b""
        scheme = "http" if self.config.endpoint.startswith("http://") else "https"

        attempts = max(0, int(self.config.max_retries)) + 1
        last_error: ApiError | None = None
        for attempt in range(attempts):
            # Build the header set cps1 signs. Fresh nonce + date per attempt
            # (anti-replay, 07§4.2 — a retried request is a new request).
            headers: dict[str, str] = {"host": host}
            for k, v in req.extra_headers.items():
                headers[k] = v
            headers[HEADER_NONCE] = "sdk-" + str(uuid.uuid4())

            date_value = _now_utc_datevalue()
            headers[HEADER_DATE] = date_value
            signed = sign(
                method=method,
                host=host,
                path=path,
                query=normalized_query,
                headers=headers,
                body=body,
                creds=creds,
                region=self.config.region,
                service=service,
                date_value=date_value,
            )

            # The URL on the wire uses the SAME RFC 3986 encoding the signature
            # canonicalised (space -> %20, uppercase hex), so the gateway
            # re-derives an identical canonical request.
            url = scheme + "://" + host + _canonical_uri(path)
            if normalized_query:
                url += "?" + _canonical_query_string(normalized_query)

            request = urllib.request.Request(
                url, data=body if body else None, method=method
            )
            for k, v in signed.headers.items():
                request.add_header(k, v)
            request.add_header("Authorization", signed.authorization)

            try:
                with _open_request(request, self.config.timeout) as resp:
                    raw = resp.read()
                    return ApiResponse(
                        status_code=resp.status,
                        headers={k: v for k, v in resp.headers.items()},
                        body=raw,
                    )
            except urllib.error.HTTPError as e:
                raw = e.read()
                resp_headers = {k: v for k, v in (e.headers or {}).items()}
                err = _parse_error(raw, e.code, resp_headers)
                retryable = e.code == 429 or e.code >= 500
                if retryable and attempt < attempts - 1:
                    last_error = err
                    time.sleep(
                        _retry_sleep_seconds(
                            attempt, resp_headers.get("Retry-After"), self.config
                        )
                    )
                    continue
                raise err from e
            except urllib.error.URLError as e:
                raise ApiError("Common.InternalError", 0, str(e)) from e

        # Unreachable in practice (loop either returns or raises), kept for type
        # completeness.
        assert last_error is not None
        raise last_error


def _parse_error(
    body: bytes, status: int, headers: Mapping[str, str] | None = None
) -> ApiError:
    """Decode a non-2xx body into the unified error model (03§9.3).

    The RequestId is preserved (body ``RequestId`` field first, then the
    ``x-cps-request-id`` / ``x-request-id`` response headers) so a failed call
    stays traceable end-to-end.
    """
    try:
        env = json.loads(body.decode("utf-8")) if body else {}
    except (ValueError, UnicodeDecodeError):
        env = {}
    code = env.get("Code") if isinstance(env, dict) else None
    message = env.get("Message") if isinstance(env, dict) else None
    request_id = env.get("RequestId") if isinstance(env, dict) else None
    if not request_id and headers:
        lowered = {k.lower(): v for k, v in headers.items()}
        request_id = lowered.get("x-cps-request-id") or lowered.get("x-request-id")
    if code:
        return ApiError(code, status, message or "", request_id=request_id or "")
    return ApiError(
        "Common.InternalError",
        status,
        "HTTP %d: %s" % (status, body.decode("utf-8", "replace")[:200]),
        request_id=request_id or "",
    )


def _now_utc_datevalue() -> str:
    """Return the current UTC time as ``YYYYMMDDTHHMMSSZ``.

    Kept in the SDK (not the signing routine) so ``sign()`` stays deterministic —
    a property the golden-vector regression relies on.
    """
    import datetime as _dt

    return _dt.datetime.now(_dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
