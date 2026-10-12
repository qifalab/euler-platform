#!/usr/bin/env python3
"""Read-only deployment checks. Never logs config values or sends credentials.

Static configuration passing is not production acceptance. Real OIDC login,
SMTP delivery, AI use and recovery evidence remain explicit pending checks.
"""
from __future__ import annotations
import argparse
import base64
from datetime import datetime, timezone
import ipaddress
import json
import os
from pathlib import Path
import re
import sys
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import Request, build_opener, HTTPRedirectHandler


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def read_environment(path: Path) -> dict[str, str]:
    """Accept literal KEY=VALUE only. Never evaluate dotenv as shell commands."""
    result = {}
    for line_no, line in enumerate(path.read_text().splitlines(), 1):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if "=" not in line:
            raise ValueError(f"invalid environment line {line_no}")
        key, value = line.split("=", 1)
        key, value = key.strip(), value.strip()
        if not re.fullmatch(r"[A-Z][A-Z0-9_]*", key) or key in result:
            raise ValueError(f"invalid or duplicate environment key on line {line_no}")
        if value[:1] in ("'", '"'):
            if len(value) < 2 or value[-1] != value[0]:
                raise ValueError(f"unclosed quote on line {line_no}")
            value = value[1:-1]
        if "${" in value or "$(" in value or "`" in value:
            raise ValueError(f"environment expansion is not supported on line {line_no}")
        result[key] = value
    return result


def valid_url(value: str, production: bool, origin_only: bool = False) -> bool:
    try:
        url = urlsplit(value)
        if not url.hostname or url.username or url.password or url.query or url.fragment:
            return False
        if origin_only and url.path not in ("", "/"):
            return False
        if url.scheme == "https":
            return True
        return not production and url.scheme == "http" and (url.hostname == "localhost" or ipaddress.ip_address(url.hostname).is_loopback)
    except ValueError:
        return False


def config_checks(env: dict[str, str], production: bool) -> list[dict]:
    results = []
    def check(ident, passed, message, missing=False):
        results.append({"id": ident, "status": "passed" if passed else "pending" if missing else "failed", "detail": message})
    try:
        key_valid = len(base64.b64decode(env.get("EULER_ENCRYPTION_KEY", ""), validate=True)) == 32
    except ValueError:
        key_valid = False
    check("encryption-key", key_valid, "A persistent base64 encoded 32-byte key is required; keep a separate protected copy.")
    operations = env.get("EULER_OPERATIONS_TOKEN", "")
    check("operations-token", len(operations) >= 32, "Set a distinct high-entropy read-only monitoring token and configure a protected scraper.", missing=not operations)
    public = env.get("EULER_PUBLIC_URL", "")
    check("public-origin", valid_url(public, production, True), "Production requires an HTTPS origin without credentials, path, query or fragment.")
    check("loopback-policy", not production or env.get("EULER_ALLOW_INSECURE_LOOPBACK", "false").lower() != "true", "Insecure loopback must be disabled in production.")
    mode = env.get("EULER_IDENTITY_MODE", "oidc")
    check("identity-mode", mode in ("oidc", "oauth2_userinfo"), "Use the platform's configured identity mode.")
    issuer = env.get("EULER_OIDC_ISSUER", "")
    check("identity-provider", valid_url(issuer, production), "Set the exact registered identity issuer.")
    method = env.get("EULER_OIDC_TOKEN_AUTH_METHOD", "client_secret_basic")
    check("identity-client", bool(env.get("EULER_OIDC_CLIENT_ID")) and (method == "none" or bool(env.get("EULER_OIDC_CLIENT_SECRET"))) and method in ("none", "client_secret_basic", "client_secret_post"), "Provide a dedicated Euler client and its required client authentication.")
    callback = env.get("EULER_OIDC_REDIRECT_URL") or public.rstrip("/") + "/auth/callback"
    check("identity-callback", callback == public.rstrip("/") + "/auth/callback" and valid_url(callback, production), "Register the exact same-origin /auth/callback URL upstream.")
    if mode == "oauth2_userinfo":
        for name in ("AUTHORIZATION", "TOKEN", "USERINFO"):
            check("oauth2-" + name.lower(), valid_url(env.get("EULER_OAUTH2_" + name + "_ENDPOINT", ""), production), "Configure the fixed HTTPS OAuth2 endpoint.")
    try:
        admins = json.loads(env.get("EULER_PLATFORM_ADMIN_IDENTITIES", "[]"))
        admins_valid = isinstance(admins, list) and bool(admins) and all(isinstance(a, dict) and a.get("provider") == issuer and isinstance(a.get("subject"), str) and bool(a["subject"].strip()) for a in admins)
    except (ValueError, TypeError):
        admins_valid = False
    check("platform-operators", admins_valid, "Configure at least one exact provider + subject; review/admin access remains separately granted.")
    proxies = env.get("EULER_WEAUTH_TRUSTED_PROXIES", "")
    try:
        safe_proxies = all(ipaddress.ip_network(item.strip(), strict=False).prefixlen > 0 for item in proxies.split(",") if item.strip())
    except ValueError:
        safe_proxies = False
    check("trusted-proxies", safe_proxies, "Use actual controlled proxy CIDRs; never a /0 network.")
    for engine in ("MYSQL", "POSTGRES"):
        configured = bool(env.get(f"EULER_DATABASE_{engine}_DSN"))
        check("database-" + engine.lower(), configured and bool(env.get(f"EULER_DATABASE_{engine}_PUBLIC_HOST")), "Database provisioning needs an Euler-owned engine and a reachable client endpoint.", missing=not configured)
    s3_keys = ("EULER_STORAGE_S3_ENDPOINT", "EULER_STORAGE_S3_ACCESS_KEY", "EULER_STORAGE_S3_SECRET_KEY", "EULER_STORAGE_S3_PUBLIC_ENDPOINT")
    s3_configured = any(env.get(key) for key in s3_keys)
    check("object-storage", all(env.get(key) for key in s3_keys) and (not production or env.get("EULER_STORAGE_S3_SECURE", "true").lower() == "true"), "Configure endpoint, access key, secret key, public endpoint and production TLS.", missing=not s3_configured)
    if env.get("EULER_TRUST_MATERIAL_S3_BUCKET"):
        trust = {key: env.get("EULER_TRUST_MATERIAL_S3_" + key) or env.get("EULER_STORAGE_S3_" + key, "")
                 for key in ("ENDPOINT", "ACCESS_KEY", "SECRET_KEY", "SECURE")}
        different_endpoint = bool(env.get("EULER_TRUST_MATERIAL_S3_ENDPOINT")) and env["EULER_TRUST_MATERIAL_S3_ENDPOINT"] != env.get("EULER_STORAGE_S3_ENDPOINT")
        credentials_bound = not different_endpoint or bool(env.get("EULER_TRUST_MATERIAL_S3_ACCESS_KEY") and env.get("EULER_TRUST_MATERIAL_S3_SECRET_KEY"))
        check("trust-private-material-storage", all(trust[key] for key in ("ENDPOINT", "ACCESS_KEY", "SECRET_KEY")) and credentials_bound and (not production or trust["SECURE"].lower() != "false"),
              "Trust requires a dedicated private bucket, valid service credentials and TLS. A distinct endpoint needs explicit matching credentials; include every material ledger bucket in recovery.")
    smtp = bool(env.get("EULER_EID_SMTP_ADDRESS"))
    check("smtp-defaults", smtp and bool(env.get("EULER_EID_SMTP_FROM")), "Configure deployment SMTP or review encrypted per-project settings; real delivery still requires acceptance.", missing=not smtp)
    check("ai-project-settings", False, "Review project-scoped AI credentials, destination, privacy settings and a real investigation; config alone cannot verify them.", missing=True)
    check("real-identity-login", False, "Complete a real login/logout, member invite, revoked-access and callback failure exercise with the production identity provider.", missing=True)
    check("recovery-drill", False, "Restore a coordinated backup to isolated SQL/S3 and app volumes, then inspect actual records and objects.", missing=True)
    return results


def http_probe(base: str, path: str) -> tuple[int, dict, bytes]:
    request = Request(base.rstrip("/") + path, headers={"Accept": "application/json", "User-Agent": "euler-deployment-check/1"})
    try:
        with build_opener(NoRedirect).open(request, timeout=10) as response:
            return response.status, dict(response.headers.items()), response.read(1024 * 1024 + 1)
    except HTTPError as error:
        return error.code, dict(error.headers.items()), error.read(1024 * 1024 + 1)


def acceptance_checks(base: str, allow_loopback: bool = False) -> list[dict]:
    if not valid_url(base, not allow_loopback, True):
        raise ValueError("acceptance URL must be an HTTPS origin (or explicit loopback preview)")
    checks = []
    for name, path, expected in (("liveness", "/healthz", {200}), ("readiness", "/readyz", {200}),
                                 ("unauthenticated-projects", "/api/v1/projects", {401}),
                                 ("public-session", "/api/v1/session", {200})):
        try:
            code, headers, body = http_probe(base, path)
            passed = code in expected and len(body) <= 1024 * 1024
            if name == "public-session" and passed:
                document = json.loads(body)
                passed = document.get("authenticated") is False
            checks.append({"id": name, "status": "passed" if passed else "failed", "httpStatus": code,
                           "detail": "Read-only HTTP check; response bodies and credentials are omitted."})
        except (OSError, ValueError, URLError):
            checks.append({"id": name, "status": "failed", "detail": "Endpoint unavailable, invalid TLS, timeout or unexpected response."})
    for ident in ("real-oidc-login", "real-smtp-delivery", "real-ai-investigation", "sql-data-restore", "s3-object-restore", "business-workflow", "revocation-and-project-isolation"):
        checks.append({"id": ident, "status": "pending", "detail": "Run the corresponding authenticated acceptance scenario and retain evidence; this probe cannot attest to it."})
    return checks


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    config = commands.add_parser("config")
    config.add_argument("--env-file", type=Path, required=True)
    config.add_argument("--preview", action="store_true")
    acceptance = commands.add_parser("acceptance")
    acceptance.add_argument("--base-url", required=True)
    acceptance.add_argument("--allow-loopback", action="store_true")
    for command in (config, acceptance):
        command.add_argument("--output", type=Path)
        command.add_argument("--strict", action="store_true", help="return 2 while any external acceptance remains pending")
    args = parser.parse_args()
    try:
        if args.command == "config":
            checks = config_checks(read_environment(args.env_file), not args.preview)
            if not args.preview and args.env_file.stat().st_mode & 0o077:
                checks.append({"id": "secret-file-permissions", "status": "failed", "detail": "The environment file must not be readable by group or others (chmod 600)."})
        else:
            checks = acceptance_checks(args.base_url, args.allow_loopback)
        failed = any(c["status"] == "failed" for c in checks)
        pending = any(c["status"] == "pending" for c in checks)
        result = {"checkedAt": datetime.now(timezone.utc).isoformat(), "phase": args.command,
                  "status": "blocked" if failed else "pending-acceptance" if pending else "passed", "productionAccepted": False, "checks": checks}
        text = json.dumps(result, indent=2, ensure_ascii=False) + "\n"
        if args.output:
            fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, "w") as output:
                output.write(text)
        print(text, end="")
        return 1 if failed else 2 if pending and args.strict else 0
    except (OSError, ValueError) as exc:
        print(f"Deployment check failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
