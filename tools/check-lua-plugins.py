"""Structural checker for the APISIX Lua plugins.

There is no Lua interpreter on this machine, so this checks the mechanical
properties a parser would catch: balanced block keywords, balanced delimiters,
and the APISIX module contract (check_schema + return _M).

Not a substitute for `luacheck` or an actual load — run those in CI once the
container toolchain is available.
"""
import glob
import re
import sys


def strip_noise(src: str) -> str:
    """Remove comments and string literals so keywords inside them don't count."""
    out = []
    for line in src.split("\n"):
        s = re.sub(r"--.*$", "", line)              # line comments
        s = re.sub(r'"(?:[^"\\]|\\.)*"', '""', s)   # double-quoted strings
        s = re.sub(r"'(?:[^'\\]|\\.)*'", "''", s)   # single-quoted strings
        out.append(s)
    return "\n".join(out)


def check(path: str) -> list:
    src = open(path, encoding="utf-8").read()
    clean = strip_noise(src)
    problems = []

    n_function = len(re.findall(r"\bfunction\b", clean))
    # `\bif\b` never matches inside `elseif` (no word boundary between "e"
    # and "if"), and elseif opens no block of its own — so count plain ifs only.
    n_if = len(re.findall(r"\bif\b", clean))
    n_for = len(re.findall(r"\bfor\b", clean))
    n_while = len(re.findall(r"\bwhile\b", clean))
    n_end = len(re.findall(r"\bend\b", clean))
    expected_end = n_function + n_if + n_for + n_while

    if expected_end != n_end:
        problems.append(
            "block keywords open {} but found {} 'end' "
            "(function={} if={} for={} while={})".format(
                expected_end, n_end, n_function, n_if, n_for, n_while
            )
        )

    for name, opener, closer in (("parentheses", "(", ")"),
                                 ("brackets", "[", "]"),
                                 ("braces", "{", "}")):
        delta = clean.count(opener) - clean.count(closer)
        if delta:
            problems.append("unbalanced {}: {:+d}".format(name, delta))

    # APISIX plugin module contract.
    if not re.search(r"return\s+_M\s*$", src.strip()):
        problems.append("missing 'return _M' at end of module")
    if "check_schema" not in src:
        problems.append("missing check_schema (APISIX requires it)")
    if not re.search(r"local\s+_M\s*=\s*\{", src):
        problems.append("missing module table declaration")
    if not re.search(r"priority\s*=\s*\d+", src):
        problems.append("missing priority (determines chain order)")

    return problems


IDENTITY_PREFIXES_RE = re.compile(r"IDENTITY_HEADER_PREFIXES\s*=\s*\{([^}]*)\}")
IDENTITY_HEADER_RE = re.compile(r'"(x-eu[a-z0-9-]*)"', re.IGNORECASE)


def check_identity_strip(files: list) -> list:
    """Every X-Euler-*/X-Eu-* header the plugins name must be stripped from
    client input by eu-auth. A prefix that silently stops matching (as "x-eu-"
    did for X-Euler-* after the rebrand) lets clients forge identity headers.
    """
    auth = [f for f in files if f.endswith("eu-auth.lua")]
    if not auth:
        return ["eu-auth.lua not found"]
    src = open(auth[0], encoding="utf-8").read()
    m = IDENTITY_PREFIXES_RE.search(src)
    if not m:
        return ["eu-auth.lua: IDENTITY_HEADER_PREFIXES table not found"]
    prefixes = [p.lower() for p in re.findall(r'"([^"]+)"', m.group(1))]
    problems = []
    if not re.search(r"ngx\.req\.get_headers\(\s*0\s*\)", src):
        problems.append("eu-auth.lua: strip must call ngx.req.get_headers(0) "
                        "(the default stops at 100 headers)")
    for path in files:
        for code_line in open(path, encoding="utf-8").read().split("\n"):
            code_line = re.sub(r"--.*$", "", code_line)
            for name in IDENTITY_HEADER_RE.findall(code_line):
                low = name.lower()
                if low in prefixes:
                    continue
                if not any(low.startswith(p) for p in prefixes):
                    problems.append("{}: header {!r} is not covered by "
                                    "IDENTITY_HEADER_PREFIXES {}".format(path, name, prefixes))
    return problems


def main() -> int:
    files = sorted(glob.glob("deploy/gitops-manifests/platform/apisix/plugins/*.lua"))
    if not files:
        print("no plugin files found")
        return 1

    failures = 0
    strip_problems = check_identity_strip(files)
    if strip_problems:
        failures += 1
        print("  FAIL  identity-header strip coverage")
        for p in strip_problems:
            print("        {}".format(p))
    else:
        print("  OK    identity-header strip coverage")
    for path in files:
        problems = check(path)
        if problems:
            failures += 1
            print("  FAIL  {}".format(path))
            for p in problems:
                print("        {}".format(p))
        else:
            print("  OK    {}".format(path))

    print()
    print("{} plugin(s), {} with issues".format(len(files), failures))
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
