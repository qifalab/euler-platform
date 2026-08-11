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
    n_if = len(re.findall(r"\bif\b", clean)) - len(re.findall(r"\belseif\b", clean))
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


def main() -> int:
    files = sorted(glob.glob("deploy/gitops-manifests/platform/apisix/plugins/*.lua"))
    if not files:
        print("no plugin files found")
        return 1

    failures = 0
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
