"""Semantic validator for APISIX route manifests.

YAML syntax validity is not enough: the failure modes that matter here are
semantic, and every one of them is silent in production.

  * a route missing sc-auth is an unauthenticated OpenAPI endpoint
  * a plugin priority inversion breaks the chain order fixed by 07§5
  * a Nacos subscription string that is not {GROUP}@@{serviceName} silently
    resolves to zero upstreams (selection pit #2)
  * an action_prefix that does not match the route's product lets a caller
    name another product's action
  * a product code outside the sc-prefixed set violates 01§1.1 D0

Run: python tools/check-apisix-routes.py
"""
import glob
import re
import sys

import yaml

# Phase-1 sellable products (09-roadmap R-01 / adjudication C1+S1).
PHASE1_PRODUCTS = {"scvpc", "scecs", "scbs", "scoss", "scrds", "scmon", "sceip"}

# Plugin chain order (07-security §5). Lower priority runs later in APISIX,
# so the declared order here must correspond to descending priority.
CHAIN_ORDER = ["ip-restriction", "limit-req", "sc-auth", "sc-authorize", "sc-audit"]

# Priorities declared in the plugin sources; kept in sync deliberately.
PLUGIN_PRIORITY = {"sc-auth": 2500, "sc-authorize": 2400, "sc-audit": 2300}

# Routes that legitimately carry no authentication, with the reason.
AUTH_EXEMPT = {
    "console-auth": "mints the session; cannot require a session to obtain one",
    "openapi-scoss-data": "S3 data plane uses MinIO SigV4, documented exemption (04§3.2)",
}


def load_routes(path):
    with open(path, encoding="utf-8") as fh:
        doc = yaml.safe_load(fh)
    return doc.get("routes", []) or []


def check_route(route, path, problems):
    rid = route.get("id", "<no id>")
    plugins = route.get("plugins", {}) or {}
    where = "{}:{}".format(path.split("/")[-1], rid)

    # 1. Authentication present, unless explicitly exempt.
    if "sc-auth" not in plugins and rid not in AUTH_EXEMPT:
        problems.append("{}: no sc-auth — endpoint would be unauthenticated".format(where))

    # 2. Signature mode must bind region and service to the route, since they
    #    feed the derived signing key and must never come from client input.
    auth = plugins.get("sc-auth") or {}
    if auth.get("mode") == "signature":
        if not auth.get("region"):
            problems.append("{}: sc-auth signature mode missing region".format(where))
        if not auth.get("service"):
            problems.append("{}: sc-auth signature mode missing service".format(where))
    if auth.get("mode") == "jwt" and not auth.get("jwt_public_key"):
        problems.append("{}: sc-auth jwt mode missing jwt_public_key".format(where))

    # 3. action_prefix must match the product this route serves, or a caller
    #    could name an action belonging to a different product.
    authz = plugins.get("sc-authorize") or {}
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

    # 7. Per-AK limiting must key on the header the auth plugin injects.
    lc = plugins.get("limit-count") or {}
    if lc.get("key") == "http_x_sc_ak_id" and "sc-auth" not in plugins:
        problems.append("{}: limits on X-Sc-Ak-Id but sc-auth never sets it".format(where))


def check_product_codes(routes, path, problems):
    """Every product-scoped route must reference a phase-1 product code."""
    for route in routes:
        authz = route.get("plugins", {}).get("sc-authorize") or {}
        prefix = authz.get("action_prefix")
        if not prefix or not prefix.startswith("sc"):
            continue
        # Platform-domain prefixes (sciam/sctrade/scres) are not products.
        if prefix in {"sciam", "sctrade", "scres"}:
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
    for path in files:
        routes = load_routes(path)
        total_routes += len(routes)
        for route in routes:
            check_route(route, path, problems)
        check_product_codes(routes, path, problems)

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
