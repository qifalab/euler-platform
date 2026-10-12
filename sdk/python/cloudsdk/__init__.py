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
a body shaped like ``{RequestId, Code, Message}`` (the OpenAPI facade's error
envelope) surfaces an :class:`ApiError` — or the subclass its code names, e.g.
:class:`ResourceNotFoundError` for ``InvalidInstanceId.NotFound`` — carrying the
code, message, HTTP status and RequestId.

Usage::

    client = Client(Config(ak="EU...", sk="...", region="cn-north-1"))
    data = client.invoke("euecs", "RunInstances",
                         {"ImageId": "img-001", "InstanceType": "s2.large"})
    instance_id = data["InstanceId"]

:meth:`Client.invoke` sends Action/Version in the query and the parameters as a
JSON body (or a form body), unwraps the success envelope
``{RequestId, Code: "OK", Data}`` and gives a create Action a ``ClientToken``
so the SDK's retries are idempotent. :meth:`Client.call` is the raw signed
round trip.
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
    "InvalidParameterError",
    "AuthorizationError",
    "ResourceNotFoundError",
    "IncorrectStatusError",
    "IdempotentParameterMismatchError",
    "QuotaExceededError",
    "InsufficientBalanceError",
    "ThrottlingError",
    "ServerError",
    "Credentials",
    "SignedRequest",
    "sign",
    "new_client_token",
    "product_api_host",
    "service_namespace",
    "ALGORITHM",
    "API_VERSION",
]

# Environment fallback for credentials. The platform's tools grew three
# spellings — this SDK's EULER_ACCESS_KEY_ID/EULER_SECRET_ACCESS_KEY, the
# Terraform provider's EULER_ACCESS_KEY/EULER_SECRET_KEY and the short
# EULER_AK/EULER_SK — so each is accepted, first non-empty wins in this order
# (the Terraform provider reads the same set).
ENV_ACCESS_KEY_ID = "EULER_ACCESS_KEY_ID"
ENV_SECRET_ACCESS_KEY = "EULER_SECRET_ACCESS_KEY"
ENV_ACCESS_KEY_ID_ALIASES = (ENV_ACCESS_KEY_ID, "EULER_ACCESS_KEY", "EULER_AK")
ENV_SECRET_ACCESS_KEY_ALIASES = (ENV_SECRET_ACCESS_KEY, "EULER_SECRET_KEY", "EULER_SK")

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

# The API version Client.invoke sends (date-type Version parameter, 04§3.2).
API_VERSION = "2026-08-01"

# Actions that buy or create a resource: Client.invoke gives them a ClientToken
# when the caller did not, so a retried create returns the resource the first
# attempt made (the facade's idempotency key) instead of buying a second one.
CREATE_ACTION_PREFIXES = ("Create", "Run", "Allocate")


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
    """Unified platform error (03§9.3). Carries the business code + HTTP status.

    Raised as the subclass the code names (see :func:`_error_class`), so callers
    can catch a category (``except ResourceNotFoundError``) or every API error
    (``except ApiError``).
    """

    def __init__(
        self, code: str, http_status: int, message: str, request_id: str = ""
    ) -> None:
        text = "%s: %s" % (code, message)
        if request_id:
            text += " (RequestId %s)" % request_id
        super().__init__(text)
        self.code = code
        self.http_status = http_status
        self.message = message
        self.request_id = request_id


class InvalidParameterError(ApiError):
    """A parameter is missing or not valid (MissingParameter, InvalidParameter,
    InvalidCidrBlock, InvalidInstanceType.NotFound, InvalidAction.NotFound, ...)."""


class AuthorizationError(ApiError):
    """The request was not authenticated or not authorized (401/403)."""


class ResourceNotFoundError(ApiError):
    """The addressed resource does not exist (404, e.g. InvalidInstanceId.NotFound)."""


class IncorrectStatusError(ApiError):
    """The resource's status does not allow the operation (IncorrectInstanceStatus,
    ConcurrentModification, ...)."""


class IdempotentParameterMismatchError(ApiError):
    """The ClientToken was already used with different parameters."""


class QuotaExceededError(ApiError):
    """The quota for the resource type is exceeded."""


class InsufficientBalanceError(ApiError):
    """The account balance cannot pay for the order."""


class ThrottlingError(ApiError):
    """The request was throttled (429)."""


class ServerError(ApiError):
    """A temporary or internal failure of the service (5xx: ServiceUnavailable,
    InternalError); safe to retry — with the same ClientToken for a create."""


def _error_class(code: str, status: int) -> type:
    """The ApiError subclass for a facade/platform error code and HTTP status."""
    if code == "IdempotentParameterMismatch" or code == "Order.InvalidClientToken":
        return IdempotentParameterMismatchError
    if code == "InsufficientBalance" or code.endswith(".InsufficientBalance"):
        return InsufficientBalanceError
    if code == "QuotaExceeded" or code.startswith("Quota."):
        return QuotaExceededError
    if status == 429 or code in ("Throttling", "Common.Throttling", "Common.Throttled"):
        return ThrottlingError
    if code.startswith("Incorrect") or code in ("ConcurrentModification", "Resource.IncorrectStatus"):
        return IncorrectStatusError
    if status == 404:
        return ResourceNotFoundError
    if status in (401, 403):
        return AuthorizationError
    if status >= 500:
        return ServerError
    if status == 400 or code in ("MissingParameter", "InvalidParameter", "Common.InvalidParameter"):
        return InvalidParameterError
    return ApiError


def _api_error(code: str, status: int, message: str, request_id: str = "") -> ApiError:
    return _error_class(code, status)(code, status, message, request_id=request_id)


def new_client_token() -> str:
    """A fresh idempotency token for one create (at most 64 printable ASCII)."""
    return "sdk-" + uuid.uuid4().hex


def _form_value(value: object) -> str:
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, (dict, list, tuple)):
        return json.dumps(value, separators=(",", ":"), ensure_ascii=False)
    return str(value)


def _encode_params(params: Mapping[str, object], form: bool) -> tuple[bytes, str]:
    """Encode Action parameters the way the facade parses a body.

    JSON (default): one object; lists stay arrays. Form: a list becomes a
    repeated key (the facade reads repeated keys as a list), nested values
    their JSON text. ``None`` means "not supplied" and is dropped.
    """
    clean = {k: v for k, v in params.items() if v is not None}
    if not form:
        body = json.dumps(clean, separators=(",", ":"), ensure_ascii=False)
        return body.encode("utf-8"), "application/json"
    pairs: list[tuple[str, str]] = []
    for k, v in clean.items():
        if isinstance(v, (list, tuple)):
            pairs.extend((k, _form_value(item)) for item in v if item is not None)
        else:
            pairs.append((k, _form_value(v)))
    return urllib.parse.urlencode(pairs).encode("ascii"), "application/x-www-form-urlencoded"


def _parse_success(resp: "ApiResponse") -> dict:
    """Unwrap the success envelope ``{RequestId, Code: "OK", Data}``.

    A 2xx whose body is not the envelope, or whose Code is not OK, is an error
    (never silently returned as data).
    """
    try:
        env = json.loads(resp.body.decode("utf-8")) if resp.body else None
    except (ValueError, UnicodeDecodeError):
        env = None
    if not isinstance(env, dict):
        raise ApiError(
            "Common.InternalError",
            resp.status_code,
            "the response is not the API envelope: %s"
            % resp.body.decode("utf-8", "replace")[:200],
        )
    code = env.get("Code") or "OK"
    request_id = env.get("RequestId") or ""
    if code != "OK":
        raise _api_error(code, resp.status_code, env.get("Message") or "", request_id)
    data = env.get("Data")
    if data is None:
        return {}
    if not isinstance(data, dict):
        raise ApiError(
            "Common.InternalError", resp.status_code,
            "the response Data is not an object", request_id=request_id,
        )
    return data


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


def _first_env(names) -> str:
    for name in names:
        value = os.environ.get(name, "")
        if value:
            return value
    return ""


def _resolve_credentials(config: Config) -> Credentials:
    """Config credentials, falling back to the environment.

    AK: EULER_ACCESS_KEY_ID, EULER_ACCESS_KEY, EULER_AK; SK:
    EULER_SECRET_ACCESS_KEY, EULER_SECRET_KEY, EULER_SK (first non-empty).
    """
    ak = config.ak or _first_env(ENV_ACCESS_KEY_ID_ALIASES)
    sk = config.sk or _first_env(ENV_SECRET_ACCESS_KEY_ALIASES)
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

    def invoke(
        self,
        product_code: str,
        action: str,
        params: Mapping[str, object] | None = None,
        *,
        version: str = API_VERSION,
        client_token: str | None = None,
        form: bool = False,
    ) -> dict:
        """Call one OpenAPI Action and return the ``Data`` of its envelope.

        Action and Version go in the query (the gateway authorizes the query's
        Action); ``params`` go in the body — a JSON object by default, a form
        body with ``form=True``. ``client_token`` is sent as ``ClientToken``;
        without one (in either place) a create Action (Create*/Run*/Allocate*)
        gets a fresh token, so the retries :meth:`call` makes on 429/5xx are
        idempotent. Errors raise :class:`ApiError` subclasses carrying code,
        message and RequestId.
        """
        body_params = dict(params or {})
        if client_token is not None:
            body_params["ClientToken"] = client_token
        elif "ClientToken" not in body_params and action.startswith(CREATE_ACTION_PREFIXES):
            body_params["ClientToken"] = new_client_token()
        body, content_type = _encode_params(body_params, form)
        resp = self.call(
            ApiRequest(
                product_code=product_code,
                method="POST",
                path="/",
                query={"Action": action, "Version": version},
                body=body,
                extra_headers={"Content-Type": content_type},
            )
        )
        return _parse_success(resp)

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
        return _api_error(code, status, message or "", request_id or "")
    return _api_error(
        "Common.InternalError",
        status,
        "HTTP %d: %s" % (status, body.decode("utf-8", "replace")[:200]),
        request_id or "",
    )


def _now_utc_datevalue() -> str:
    """Return the current UTC time as ``YYYYMMDDTHHMMSSZ``.

    Kept in the SDK (not the signing routine) so ``sign()`` stays deterministic —
    a property the golden-vector regression relies on.
    """
    import datetime as _dt

    return _dt.datetime.now(_dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
