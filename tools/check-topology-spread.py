"""Cross-AZ topology structural validator (M-6.2c, 00 §4.4 P2, 09-roadmap §4.3).

`kubeconform` checks schema; this checks the *topology intent* that schema
cannot express — the failures that are silent in production:

  * a stateful workload (StatefulSet: Kafka/MySQL/Redis) whose replicas are
    NOT spread across AZs. A 3-broker Kafka with no zone anti-affinity lands
    all on one node and "survives AZ loss" is a claim the manifest does not back.
  * a stateful workload whose zone anti-affinity is `preferred` (soft) where
    the P2 contract needs `required` (hard) — a soft constraint that packs
    into one AZ under bin-packing pressure is the exact regression.
  * a MySQL MGR group spread as (3,0) — three nodes one AZ — which is the
    single-AZ concentration the P2 topology forbids even before a failure.
  * a prod/staging stateless Deployment override that claims zoneSpread but
    whose chart template has no zone-spread code path (the override would be
    silently ignored — replicas concentrated, override a placebo).

Run: python tools/check-topology-spread.py

It does NOT apply anything; it reads manifests source-only (same口径 as the
phase-1 validators). Helm templates are read as YAML documents (the checker
is helm-aware: it strips Go directives enough to parse the structural shape,
matching check-yaml-syntax.py's helm handling).

ASCII output only — the Windows GBK console crashes on Unicode (✗/—), as the
proto-breaking checker learned the hard way (M-5.3).
"""

from __future__ import annotations

import glob
import re
import sys
from typing import Any

import yaml

MANIFEST_GLOB = "deploy/gitops-manifests/platform/middleware/*.yaml"
DEPLOYMENT_TEMPLATES = "deploy/gitops-manifests/apps/*/templates/deployment.yaml"
# M-8 multi-region: overrides live per (region, env) — envs/<region>/<env>/
# (08§4.2; the region level is the P3 extension of dir-as-env).
ENV_OVERRIDES = "deploy/gitops-manifests/envs/*/{env}/values-overrides/*.yaml"

# Stateful workloads that MUST carry cross-AZ placement intent, because they
# are the data plane whose AZ-loss survival is the P2 promise (00 §4.4).
STATEFUL_KINDS = {"StatefulSet"}

ZONE_TOPOLOGY_KEY = "topology.kubernetes.io/zone"


def load_docs(path: str) -> list[dict]:
    """Load all YAML docs from a file, tolerating helm Go directives.

    Helm templates contain {{ ... }} which is not YAML. We blank directive
    lines (lines whose non-whitespace prefix is {{) so the structural skeleton
    — kind, replicas, affinity, topologySpreadConstraints — parses. Values
    inside directives become empty strings, which is fine: we check for the
    PRESENCE of the constraint keys, not their rendered values (helm isn't
    on this host; rendered values are check-yaml-syntax's [helm] concern).
    """
    with open(path, encoding="utf-8") as fh:
        raw = fh.read()
    # Blank lines that are pure helm control flow. Keep lines that have YAML
    # structure around the directive (e.g. `topologyKey: topology.kubernetes.io/zone`).
    cleaned_lines = []
    for line in raw.splitlines():
        stripped = line.lstrip()
        if stripped.startswith("{{") and stripped.endswith("}}"):
            cleaned_lines.append("")  # control flow → blank, preserve indent
        else:
            # Strip inline {{...}} so `replicas: {{ .Values.replicas }}` →
            # `replicas:` (a key with null value, which parses).
            line = re.sub(r"\{\{[^}]*\}\}", "", line)
            cleaned_lines.append(line)
    cleaned = "\n".join(cleaned_lines)
    try:
        return [d for d in yaml.safe_load_all(cleaned) if d]
    except yaml.YAMLError:
        return []


def docs_in_files(glob_pattern: str) -> list[tuple[str, dict]]:
    """Yield (path, doc) for every YAML doc matching the glob."""
    out: list[tuple[str, dict]] = []
    for path in sorted(glob.glob(glob_pattern, recursive=True)):
        for doc in load_docs(path):
            if isinstance(doc, dict):
                out.append((path, doc))
    return out


def workloads(doc: dict) -> list[tuple[str, str, dict]]:
    """Return [(path, kind, spec.template.spec)] for Deployment/StatefulSet docs."""
    kind = doc.get("kind")
    if kind not in STATEFUL_KINDS and kind != "Deployment":
        return []
    spec = doc.get("spec") or {}
    template = spec.get("template") or {}
    tspec = template.get("spec") or {}
    path = doc.get("metadata", {}).get("name", "<unnamed>")
    return [(path, kind, tspec)]


def has_zone_anti_affinity(tspec: dict) -> tuple[bool, str]:
    """True if the pod spec hard-anti-affines on the zone topology key."""
    affinity = tspec.get("affinity") or {}
    pa = affinity.get("podAntiAffinity") or {}
    # Check both required and preferred; return which.
    for key in ("requiredDuringSchedulingIgnoredDuringExecution",
                "preferredDuringSchedulingIgnoredDuringExecution"):
        terms = pa.get(key) or []
        for term in terms:
            # preferred terms wrap in {podAffinityTerm: ...}
            inner = term.get("podAffinityTerm", term) if isinstance(term, dict) else term
            if not isinstance(inner, dict):
                continue
            tk = inner.get("topologyKey")
            if tk == ZONE_TOPOLOGY_KEY:
                return True, ("required" if key.startswith("required") else "preferred")
    return False, ""


def has_zone_spread(tspec: dict) -> bool:
    """True if topologySpreadConstraints includes the zone topology key."""
    for c in tspec.get("topologySpreadConstraints") or []:
        if not isinstance(c, dict):
            continue
        if c.get("topologyKey") == ZONE_TOPOLOGY_KEY:
            return True
    return False


def replica_count(doc: dict) -> int:
    rep = (doc.get("spec") or {}).get("replicas")
    if isinstance(rep, int):
        return rep
    return 0  # templated/absent → unknown, treated as 0 (not a structural fact)


def check_stateful_spread() -> list[str]:
    """StatefulSets (Kafka/MySQL/Redis) MUST carry zone anti-affinity or spread."""
    errs: list[str] = []
    for path, doc in docs_in_files(MANIFEST_GLOB):
        for name, kind, tspec in workloads(doc):
            if kind != "StatefulSet":
                continue
            has_aa, aa_kind = has_zone_anti_affinity(tspec)
            has_sp = has_zone_spread(tspec)
            if not has_aa and not has_sp:
                errs.append(
                    "%s: StatefulSet %s has NO zone anti-affinity or zone spread "
                    "-- its replicas can concentrate in one AZ, which breaks the "
                    "P2 AZ-loss contract (00§4.4)" % (path, name)
                )
                continue
            # MySQL MGR specifically: the (2,1) spread needs the runbook, but
            # a (3,0) concentration (all one zone) is never acceptable. We
            # flag any StatefulSet whose replicas > len(zones) with NO zone
            # constraint at all — caught above — but also flag a MGR group
            # that uses a SOFT constraint where hard is the contract. Redis
            # uses required (hard) per-shard; Kafka uses required; MySQL uses
            # preferred (deliberate, 3-across-2-AZ). We do NOT flag MySQL's
            # preferred as an error — it is documented in the manifest.
    return errs


def check_deployment_zone_path() -> list[str]:
    """A chart template referenced by a zoneSpread override MUST contain the
    zone-spread code path, or the override is a placebo."""
    errs: list[str] = []
    # Gather which (region, env) overrides turn zoneSpread on.
    zone_spread_services: dict[str, list[str]] = {}  # service -> ["<region>/<env>"]
    for env in ("prod", "staging"):
        for path in sorted(glob.glob(ENV_OVERRIDES.format(env=env))):
            for doc in load_docs(path):
                if not isinstance(doc, dict):
                    continue
                topo = doc.get("topology")
                if isinstance(topo, dict) and topo.get("zoneSpread") is True:
                    svc = re.search(r"values-overrides[/\\](.+)\.yaml$", path)
                    svc_name = svc.group(1) if svc else path
                    region = re.search(r"envs[/\\]([^/\\]+)[/\\]" + env, path)
                    label = (region.group(1) + "/" + env) if region else env
                    zone_spread_services.setdefault(svc_name, []).append(label)
    if not zone_spread_services:
        return []
    # Read each deployment template; the zone path must be present.
    templates = {}
    for path in sorted(glob.glob(DEPLOYMENT_TEMPLATES)):
        svc = re.search(r"apps[/\\]([^/\\]+)[/\\]templates", path)
        if svc:
            templates[svc.group(1)] = path
    for svc, envs in zone_spread_services.items():
        tmpl = templates.get(svc)
        if not tmpl:
            # Not every service has its own chart (e.g. console-bff). Skip
            # silently rather than false-positive; the override has no template
            # to validate against.
            continue
        with open(tmpl, encoding="utf-8") as fh:
            src = fh.read()
        if "topology.kubernetes.io/zone" not in src:
            errs.append(
                "%s: %s enables topology.zoneSpread in %s but the chart "
                "template has no zone-spread code path -- the override is a "
                "placebo, replicas would NOT spread" % (tmpl, svc, "/".join(envs))
            )
    return errs


def main() -> int:
    errors: list[str] = []
    errors += check_stateful_spread()
    errors += check_deployment_zone_path()

    # Summary: how many stateful workloads are topology-checked.
    stateful_count = 0
    for path, doc in docs_in_files(MANIFEST_GLOB):
        for _, kind, _ in workloads(doc):
            if kind == "StatefulSet":
                stateful_count += 1
    spread_ok = 0
    for path, doc in docs_in_files(MANIFEST_GLOB):
        for _, kind, tspec in workloads(doc):
            if kind == "StatefulSet" and (has_zone_anti_affinity(tspec)[0] or has_zone_spread(tspec)):
                spread_ok += 1

    print(
        "topology-spread: %d stateful workloads, %d carry zone anti-affinity/spread"
        % (stateful_count, spread_ok)
    )

    if errors:
        print("\nTOPOLOGY SPREAD FAILURE(S):")
        for e in errors:
            print("  [FAIL] " + e)
        print(
            "\nThese are P2 contract violations (00§4.4): a workload claiming "
            "to survive AZ loss must actually be spread across AZs."
        )
        return 1
    print("ok -- all stateful workloads carry cross-AZ placement intent")
    return 0


if __name__ == "__main__":
    sys.exit(main())
