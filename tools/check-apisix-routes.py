"""Semantic validator for APISIX route manifests.

YAML syntax validity is not enough: the failure modes that matter here are
semantic, and every one of them is silent in production.

  * a route missing eu-auth is an unauthenticated OpenAPI endpoint
  * a plugin priority inversion breaks the chain order fixed by 07§5
  * a Nacos subscription string that is not {GROUP}@@{serviceName} silently
    resolves to zero upstreams (selection pit #2)
  * an action_prefix that does not match the route's product lets a caller
    name another product's action
  * a product code outside the eu-prefixed set violates 01§1.1 D0
  * a plugin missing from config.yaml's `plugins:` list (which REPLACES the
    APISIX default set) makes every route that uses it fail to load
  * a route that can forward /internal/* exposes a service-to-service
    endpoint (e.g. svc-payment's /internal/payment/consume) to the internet

Run: python tools/check-apisix-routes.py
"""
import glob
import re
import sys

import yaml

# Phase-1 sellable products (09-roadmap R-01 / adjudication C1+S1).
PHASE1_PRODUCTS = {"euvpc", "euecs", "eubs", "euoss", "eurds", "eumon", "eueip"}

# Plugin chain order (07-security §5). Lower priority runs later in APISIX,
# so the declared order here must correspond to descending priority.
CHAIN_ORDER = ["ip-restriction", "limit-req", "eu-auth", "eu-authorize", "eu-audit"]

# Priorities declared in the plugin sources; kept in sync deliberately.
PLUGIN_PRIORITY = {"eu-auth": 2500, "eu-authorize": 2400, "eu-audit": 2300}

# Routes that legitimately carry no authentication, with the reason.
AUTH_EXEMPT = {
    "console-auth": "mints the session; cannot require a session to obtain one",
    "console-auth-session": "refresh/session/logout: svc-iam authenticates the refresh cookie / bearer itself",
    "openapi-euoss-data": "S3 data plane uses MinIO SigV4, documented exemption (04§3.2)",
}

# Catch-all routes allowed to forward /internal/* because their upstream is
# not a platform service (no /internal contract behind them).
INTERNAL_EXEMPT = {
    "openapi-euoss-data": "MinIO S3 data plane; /internal/... is a path-style bucket key there",
}


def excludes_internal(route):
    """True when the route's vars reject /internal/* (["uri", "!", "~~", re])."""
    for cond in route.get("vars") or []:
        if (isinstance(cond, list) and len(cond) == 4 and cond[0] == "uri"
                and cond[1] == "!" and cond[2] in ("~~", "~*")
                and re.match(cond[3], "/internal/x")):
            return True
    return False


# The data-plane config whose `plugins:` list replaces APISIX's defaults.
CONFIG = "deploy/gitops-manifests/platform/apisix/config/config.yaml"


def load_routes(path):
    with open(path, encoding="utf-8") as fh:
        doc = yaml.safe_load(fh)
    return doc.get("routes", []) or []


def check_route(route, path, problems):
    rid = route.get("id", "<no id>")
    plugins = route.get("plugins", {}) or {}
    where = "{}:{}".format(path.split("/")[-1], rid)

    # 1. Authentication present, unless explicitly exempt.
    if "eu-auth" not in plugins and rid not in AUTH_EXEMPT:
        problems.append("{}: no eu-auth — endpoint would be unauthenticated".format(where))

    # 2. Signature mode must bind region and service to the route, since they
    #    feed the derived signing key and must never come from client input.
    auth = plugins.get("eu-auth") or {}
    # Anonymous mode establishes no identity: only exact-path, GET/HEAD-only
    # public routes may use it, and never with an authorization plugin.
    if auth.get("mode") == "anonymous":
        methods = set(route.get("methods") or [])
        if not methods or not methods <= {"GET", "HEAD"}:
            problems.append("{}: eu-auth anonymous mode must be GET/HEAD only".format(where))
        if any("*" in u for u in (route.get("uris") or [route.get("uri", "")])):
            problems.append("{}: eu-auth anonymous mode must use exact paths".format(where))
        if "eu-authorize" in plugins:
            problems.append("{}: eu-authorize on an anonymous route".format(where))
    if auth.get("mode") == "signature":
        if not auth.get("region"):
            problems.append("{}: eu-auth signature mode missing region".format(where))
        if not auth.get("service"):
            problems.append("{}: eu-auth signature mode missing service".format(where))
    # jwt_public_key_file is the K8s form: the eu-auth plugin reads the
    # mounted eu-jwt-public-key Secret (route files cannot interpolate env).
    if auth.get("mode") == "jwt" and not (
        auth.get("jwt_public_key") or auth.get("jwt_public_key_file")
    ):
        problems.append(
            "{}: eu-auth jwt mode missing jwt_public_key/jwt_public_key_file".format(where)
        )

    # 3. action_prefix must match the product this route serves, or a caller
    #    could name an action belonging to a different product.
    authz = plugins.get("eu-authorize") or {}
    prefix = authz.get("action_prefix")
    if authz.get("action_from_query") and not prefix:
        problems.append("{}: action_from_query without action_prefix".format(where))
    if prefix:
        hosts = route.get("hosts") or []
        uris = " ".join(route.get("uris") or [])
        subject = " ".join(hosts) + " " + uris
        # Product-scoped routes must mention their own product code.
        if prefix in PHASE1_PRODUCTS and prefix not in subject:
            problems.append(
                "{}: action_prefix '{}' does not match route host/uri '{}'".format(
                    where, prefix, subject.strip()
                )
            )

    # 4. Nacos subscription form (selection pit #2).
    upstream = route.get("upstream") or {}
    svc = upstream.get("service_name")
    if svc is not None:
        if "@@" not in svc:
            problems.append(
                "{}: upstream service_name '{}' must be {{GROUP}}@@{{serviceName}}".format(where, svc)
            )
        else:
            group, name = svc.split("@@", 1)
            if group != name:
                problems.append(
                    "{}: Nacos Group must equal service name (04§4.3), got '{}' vs '{}'".format(
                        where, group, name
                    )
                )
        if upstream.get("discovery_type") != "nacos":
            problems.append("{}: service_name set but discovery_type is not nacos".format(where))

    # 5. Chain order: the plugins present must appear in the fixed order.
    present = [p for p in CHAIN_ORDER if p in plugins]
    priorities = [PLUGIN_PRIORITY.get(p) for p in present if p in PLUGIN_PRIORITY]
    if priorities != sorted(priorities, reverse=True):
        problems.append("{}: custom plugin priorities out of chain order: {}".format(where, present))

    # 6. Rate limiting present — an unlimited OpenAPI route is a DoS vector.
    if not ({"limit-req", "limit-count"} & set(plugins)):
        problems.append("{}: no rate limiting configured".format(where))

    # 7. Per-AK limiting must key on the header the auth plugin injects:
    #    X-Euler-Ak-Id (nginx var http_x_euler_ak_id) is set by eu-auth in
    #    signature mode only — jwt mode carries no AK. Keying on it anywhere
    #    else collapses every caller into one shared (empty-key) bucket.
    lc = plugins.get("limit-count") or {}
    if lc.get("key") == "http_x_euler_ak_id" and auth.get("mode") != "signature":
        problems.append("{}: limits on X-Euler-Ak-Id but eu-auth (signature mode) never sets it".format(where))
    # Same for the per-account key: X-Euler-Account-Id only exists after eu-auth.
    if lc.get("key") == "http_x_euler_account_id" and "eu-auth" not in plugins:
        problems.append("{}: limits on X-Euler-Account-Id but eu-auth never sets it".format(where))

    # 8. /internal/* is service-to-service only (svc-iam verify/authz,
    #    svc-payment consume/refund, ...): no route may name it, and a
    #    prefix route broad enough to cover it must exclude it via vars.
    uris = list(route.get("uris") or []) + ([route["uri"]] if route.get("uri") else [])
    for u in uris:
        if u.startswith("/internal"):
            problems.append("{}: routes service-internal path '{}'".format(where, u))
        elif (u.endswith("*") and "/internal/".startswith(u[:-1])
              and rid not in INTERNAL_EXEMPT and not excludes_internal(route)):
            problems.append(
                "{}: catch-all uri '{}' would forward /internal/*; add vars "
                "[\"uri\", \"!\", \"~~\", \"^/internal(/|$)\"]".format(where, u)
            )


def check_product_codes(routes, path, problems):
    """Every product-scoped route must reference a phase-1 product code."""
    for route in routes:
        authz = route.get("plugins", {}).get("eu-authorize") or {}
        prefix = authz.get("action_prefix")
        if not prefix or not prefix.startswith("eu"):
            continue
        # Platform-domain prefixes (euiam/eutrade/eures) are not products.
        if prefix in {"euiam", "eutrade", "eures"}:
            continue
        if prefix not in PHASE1_PRODUCTS:
            problems.append(
                "{}:{}: action_prefix '{}' is not in the phase-1 product set {}".format(
                    path.split("/")[-1], route.get("id"), prefix, sorted(PHASE1_PRODUCTS)
                )
            )


def main():
    files = sorted(glob.glob("deploy/gitops-manifests/platform/apisix/routes/*.yaml"))
    if not files:
        print("no route manifests found")
        return 1

    problems = []
    total_routes = 0
    with open(CONFIG, encoding="utf-8") as fh:
        enabled = set((yaml.safe_load(fh) or {}).get("plugins") or [])
    for path in files:
        routes = load_routes(path)
        total_routes += len(routes)
        for route in routes:
            check_route(route, path, problems)
        check_product_codes(routes, path, problems)
        with open(path, encoding="utf-8") as fh:
            rules = (yaml.safe_load(fh) or {}).get("global_rules") or []
        for item in list(routes) + list(rules):
            for name in sorted(item.get("plugins") or {}):
                if name not in enabled:
                    problems.append("{}:{}: plugin '{}' is not enabled in {}".format(
                        path.split("/")[-1], item.get("id"), name, CONFIG))

    for p in problems:
        print("  FAIL  {}".format(p))

    print()
    print("{} route manifest(s), {} route(s), {} problem(s)".format(
        len(files), total_routes, len(problems)))
    if not problems:
        print("chain order, auth coverage, Nacos form, and product codes all consistent")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
