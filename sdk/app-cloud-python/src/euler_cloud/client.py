"""Small dependency-free client for the versioned Euler machine API.

Authorization stays on the server: this client does not turn API keys into
browser sessions, elevate scopes, follow redirects, or retry mutations.
"""

from dataclasses import dataclass, field
import ipaddress
import json
import re
from typing import Any, Mapping
import urllib.error
import urllib.parse
import urllib.request


class APIError(Exception):
    """A structured API failure, with a request ID for operator correlation."""

    def __init__(self, status: int, code: str, message: str, request_id: str = ""):
        super().__init__(message)
        self.status = status
        self.code = code
        self.request_id = request_id


@dataclass(frozen=True)
class Response:
    status: int
    data: Any
    request_id: str
    content_type: str


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # A deployment mistake must not forward the service secret to another host.
        return None


def _identifier(value: str) -> str:
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,160}", value):
        raise ValueError("tenant, project and application IDs must be URL-safe identifiers")
    return value


@dataclass(repr=False)
class Client:
    base_url: str
    tenant_id: str
    project_id: str
    token: str = field(repr=False)
    timeout: float = 30
    allow_insecure_loopback: bool = False

    def __post_init__(self):
        parsed = urllib.parse.urlsplit(self.base_url)
        if (not parsed.hostname or parsed.username or parsed.password or parsed.query
                or parsed.fragment or parsed.path not in ("", "/")):
            raise ValueError("base_url must be an origin without credentials, path or query")
        try:
            loopback = ipaddress.ip_address(parsed.hostname).is_loopback
        except ValueError:
            loopback = parsed.hostname == "localhost"
        if parsed.scheme != "https" and not (
            parsed.scheme == "http" and self.allow_insecure_loopback and loopback
        ):
            raise ValueError("HTTPS is required; HTTP is only allowed for explicit loopback development")
        _identifier(self.tenant_id)
        _identifier(self.project_id)
        if not self.token.startswith("euler_sa_") or any(c.isspace() for c in self.token):
            raise ValueError("an Euler project service-account secret is required")
        if not 0 < self.timeout <= 120:
            raise ValueError("timeout must be between 0 and 120 seconds")
        self.base_url = self.base_url.rstrip("/")
        self._opener = urllib.request.build_opener(_NoRedirect())

    def __repr__(self) -> str:
        return f"Client(base_url={self.base_url!r}, tenant_id={self.tenant_id!r}, project_id={self.project_id!r}, token=<redacted>)"

    def request(self, application: str, method: str = "GET", path: str = "/", *,
                data: Any = None, query: Mapping[str, Any] | None = None) -> Response:
        """Call one permitted application route; writes are deliberately not retried.

        Pass query parameters separately. Raw bytes support file endpoints;
        ordinary Python objects are encoded as JSON. A denied operation remains
        denied even if the same account can manage another application.
        """
        _identifier(application)
        method = method.upper()
        if method not in {"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}:
            raise ValueError("unsupported HTTP method")
        parsed = urllib.parse.urlsplit(path)
        decoded = urllib.parse.unquote(path)
        if (parsed.scheme or parsed.netloc or parsed.query or parsed.fragment
                or not path.startswith("/") or path.startswith("//") or "\\" in decoded
                or any(part in (".", "..") for part in decoded.split("/"))
                or any(ord(c) < 32 for c in decoded) or "%" in decoded):
            raise ValueError("path must be a normalized application-relative path; use query= separately")
        prefix = f"/api/v1/machine/tenants/{self.tenant_id}/projects/{self.project_id}/apps/{application}"
        url = self.base_url + prefix + path
        if query:
            url += "?" + urllib.parse.urlencode(query, doseq=True)
        headers = {"Authorization": "Bearer " + self.token, "Accept": "application/json"}
        body = None
        if data is not None:
            body = data if isinstance(data, bytes) else json.dumps(data, ensure_ascii=False).encode()
            headers["Content-Type"] = "application/octet-stream" if isinstance(data, bytes) else "application/json"
        request = urllib.request.Request(url, data=body, headers=headers, method=method)
        try:
            with self._opener.open(request, timeout=self.timeout) as result:
                payload = result.read(16 * 1024 * 1024 + 1)
                if len(payload) > 16 * 1024 * 1024:
                    raise APIError(result.status, "response_too_large", "Response exceeds SDK limit; use pagination or a signed download")
                content_type = result.headers.get("Content-Type", "")
                value = json.loads(payload) if payload and "json" in content_type else payload or None
                return Response(result.status, value, result.headers.get("X-Request-ID", ""), content_type)
        except urllib.error.HTTPError as error:
            try:
                payload = error.read(64 * 1024)
            finally:
                error.close()
            try:
                detail = json.loads(payload).get("error", {})
                if not isinstance(detail, dict):
                    detail = {}
            except (ValueError, AttributeError):
                detail = {}
            raise APIError(error.code, str(detail.get("code", "http_error")),
                           str(detail.get("message", "Euler request failed")),
                           error.headers.get("X-Request-ID", "")) from None
        except (urllib.error.URLError, TimeoutError, OSError):
            # Do not include transport representations, which may contain headers.
            raise APIError(0, "transport_error", "Unable to reach Euler within the configured timeout") from None

    def databases(self):
        return self.request("database", path="/databases").data

    def buckets(self):
        return self.request("storage", path="/buckets").data

    def statistics_sites(self):
        return self.request("statistics", path="/sites").data

    def quota(self, application: str):
        if application not in {"database", "storage"}:
            raise ValueError("quota is only available for database and storage")
        return self.request(application, path="/quota").data
