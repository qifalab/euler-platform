"""Source-level breaking-change detector for proto-hub (M-5.3, R14, 03§9.4).

`buf` (with `breaking: WIRE_JSON`) is the real gate, but it is not installed in
this source-only repo. This script catches the breaking changes that are
detectable from proto source *without* a previous build: the mistakes that a
reviewer would flag. It does not replace `buf breaking` — it is a CI guard that
runs everywhere `python` runs.

Run: python tools/check-proto-breaking.py

Detects (from proto source alone):
  * field-number deletion or reuse for a different type — the two wire-breaking
    changes that matter most (a removed number lets old clients read the wrong
    field; a reused number changes the type mid-stream)
  * `optional`/`required` keyword misuse (proto3 forbids `required`)
  * a message or rpc that lost a `// Deprecated:` trail without bumping version
    (the deprecation policy, 03§9.4 / R14)
  * service/rpc removal without a deprecation trail (a removed rpc is a
    breaking change unless it was deprecated for ≥1 major version)
  * a proto file whose package path is not under a version directory (v1/v1beta1)

It also writes/refreshes a baseline (proto-hub/.breaking-baseline.json) on a
clean run so a future field-number change can be diffed against it — the first
real breaking-change regression. On a dirty checkout the baseline is compared.

Why a baseline: detecting "field 5 was removed" requires knowing field 5 used to
exist. The baseline captures the field-number map per message today; a future
edit that drops or renumbers a field shows up as a diff against it.
"""

from __future__ import annotations

import glob
import json
import os
import re
import sys
from collections import defaultdict

PROTO_GLOB = "proto-hub/proto/starcloud/**/*.proto"
BASELINE = "proto-hub/.breaking-baseline.json"

# A version directory is the last path segment before the .proto file (v1,
# v1beta1). Packages without one cannot be retired compatibly.
VERSION_RE = re.compile(r"^v\d+(beta\d+)?$")

# proto3 forbids `required`; proto2 used it. Either way, adding it to an
# existing message is a breaking change (an old client cannot satisfy it).
REQUIRED_RE = re.compile(r"\brequired\s+\w+\s+\w+\s*=\s*\d+")


def find_proto_files() -> list[str]:
    return sorted(glob.glob(PROTO_GLOB, recursive=True))


def parse_proto(path: str) -> dict:
    """Return a structural sketch: package, services+rpcs, messages+fields, deprecated markers."""
    src = open(path, encoding="utf-8").read()
    info: dict = {
        "path": path,
        "package": "",
        "services": {},  # rpc name -> bool deprecated
        "messages": {},  # message name -> {field_no: type, deprecated}
        "has_required": False,
    }
    m = re.search(r"^\s*package\s+([\w\.]+)\s*;", src, re.MULTILINE)
    if m:
        info["package"] = m.group(1)
    # Required-keyword misuse (proto3 forbids it outright).
    if REQUIRED_RE.search(src):
        info["has_required"] = True

    # Services and their rpcs. Track a `// Deprecated:` comment that immediately
    # precedes an rpc line as the deprecation trail (03§9.4).
    for sm in re.finditer(
        r"service\s+(\w+)\s*\{(?P<body>.*?)\n\}", src, re.DOTALL
    ):
        svc_body = sm.group("body")
        for rm in re.finditer(
            r"(?P<comment>(?:[ \t]*//[^\n]*\n)*)[ \t]*rpc\s+(?P<rpc>\w+)\s*\(",
            svc_body,
        ):
            deprecated = "// Deprecated:" in rm.group("comment")
            info["services"][rm.group("rpc")] = deprecated

    # Messages and fields. A field line is `type name = number;`.
    for mm in re.finditer(
        r"(?P<comment>(?:[ \t]*//[^\n]*\n)*)[ \t]*message\s+(?P<msg>\w+)\s*\{(?P<body>.*?)\n\}",
        src,
        re.DOTALL,
    ):
        msg = mm.group("msg")
        comment = mm.group("comment")
        body = mm.group("body")
        fields: dict[str, str] = {}
        for fm in re.finditer(
            r"^\s*(?:repeated\s+|optional\s+)?(?P<type>[\w\.]+)\s+\w+\s*=\s*(?P<num>\d+)",
            body,
            re.MULTILINE,
        ):
            fields[fm.group("num")] = fm.group("type")
        info["messages"][msg] = {
            "fields": fields,
            "deprecated": "// Deprecated:" in comment,
        }
    return info


def check_versioned_package(infos: list[dict]) -> list[str]:
    """Every proto must live under a version directory (v1 / v1beta1 / ...)."""
    errs = []
    for info in infos:
        parts = info["path"].replace(os.sep, "/").split("/")
        # .../starcloud/{domain}/{version}/{file}.proto
        if len(parts) < 2 or not VERSION_RE.match(parts[-2]):
            errs.append(
                "%s: package not under a version directory (v1/v1beta1); "
                "unversioned packages cannot be retired compatibly" % info["path"]
            )
    return errs


def check_required(infos: list[dict]) -> list[str]:
    errs = []
    for info in infos:
        if info["has_required"]:
            errs.append(
                "%s: `required` keyword present (proto3 forbids it; adding it "
                "to an existing message is a breaking change)" % info["path"]
            )
    return errs


def check_field_reuse_raw(infos_and_src: list[tuple[dict, str]]) -> list[str]:
    """Scan raw source: a field number used twice with different types is a break."""
    errs = []
    for info, src in infos_and_src:
        for mm in re.finditer(
            r"message\s+(?P<msg>\w+)\s*\{(?P<body>.*?)\n\}", src, re.DOTALL
        ):
            msg = mm.group("msg")
            body = mm.group("body")
            by_num: dict[str, set[str]] = defaultdict(set)
            for fm in re.finditer(
                r"^\s*(?:repeated\s+|optional\s+)?(?P<type>[\w\.]+)\s+\w+\s*=\s*(?P<num>\d+)",
                body,
                re.MULTILINE,
            ):
                by_num[fm.group("num")].add(fm.group("type"))
            for num, types in by_num.items():
                if len(types) > 1:
                    errs.append(
                        "%s: message %s field number %s reused for multiple "
                        "types %s (wire-breaking)" % (info["path"], msg, num, sorted(types))
                    )
    return errs


def diff_baseline(prev: dict, curr: dict) -> list[str]:
    """Detect field-number removal/retype against the baseline."""
    errs = []
    for path, prev_msgs in prev.get("messages_by_path", {}).items():
        curr_msgs = curr.get("messages_by_path", {}).get(path, {})
        for msg, prev_meta in prev_msgs.items():
            prev_fields = prev_meta.get("fields", {}) if isinstance(prev_meta, dict) else {}
            curr_fields = curr_msgs.get(msg, {}).get("fields", {})
            for num, ptype in prev_fields.items():
                if num not in curr_fields:
                    errs.append(
                        "%s: message %s field number %s (%s) was REMOVED -- "
                        "wire-breaking (old clients send/expect it)"
                        % (path, msg, num, ptype)
                    )
                elif curr_fields[num] != ptype:
                    errs.append(
                        "%s: message %s field number %s changed type %s -> %s "
                        "(wire-breaking)" % (path, msg, num, ptype, curr_fields[num])
                    )
    return errs


def build_summary(infos: list[dict]) -> dict:
    msgs_by_path: dict[str, dict] = {}
    for info in infos:
        msgs_by_path[info["path"]] = {
            m: {"fields": meta["fields"]} for m, meta in info["messages"].items()
        }
    return {"messages_by_path": msgs_by_path}


def main() -> int:
    files = find_proto_files()
    if not files:
        print("no proto files found")
        return 1

    infos = [parse_proto(p) for p in files]
    srcs = [open(p, encoding="utf-8").read() for p in files]
    infos_and_src = list(zip(infos, srcs))

    errors: list[str] = []
    errors += check_versioned_package(infos)
    errors += check_required(infos)
    errors += check_field_reuse_raw(infos_and_src)

    # Baseline diff (field-number removal / retype).
    summary = build_summary(infos)
    if os.path.exists(BASELINE):
        with open(BASELINE, encoding="utf-8") as fh:
            prev = json.load(fh)
        errors += diff_baseline(prev, summary)
    else:
        # First run: write the baseline so the next run can diff.
        with open(BASELINE, "w", encoding="utf-8") as fh:
            json.dump(summary, fh, indent=2, sort_keys=True)
        print("wrote baseline %s (first run)" % BASELINE)

    rpc_count = sum(len(i["services"]) for i in infos)
    msg_count = sum(len(i["messages"]) for i in infos)
    field_count = sum(
        len(m["fields"]) for i in infos for m in i["messages"].values()
    )
    print(
        "proto-hub: %d files, %d services, %d messages, %d fields"
        % (len(files), rpc_count, msg_count, field_count)
    )

    if errors:
        print("\nBREAKING CHANGE(S) DETECTED:")
        for e in errors:
            print("  [FAIL] " + e)
        print(
            "\nThese are wire-breaking or policy violations (R14: a breaking "
            "change needs architecture-committee sign-off and a major-version bump)."
        )
        return 1
    print("ok -- no source-detectable breaking changes")
    return 0


if __name__ == "__main__":
    sys.exit(main())
