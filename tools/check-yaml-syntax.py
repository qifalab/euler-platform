"""YAML syntax checker for the GitOps manifest tree.

Helm chart templates contain Go template directives, which are not valid YAML
on their own. Those files are checked by substituting placeholders for the
directives and parsing the result, so a genuine structural error is still
caught while `{{ ... }}` is tolerated.

Run: python tools/check-yaml-syntax.py
"""
import glob
import os
import re
import sys

import yaml


def is_helm_template(path: str) -> bool:
    return os.sep + "templates" + os.sep in path or "{{" in open(path, encoding="utf-8").read()


def strip_go_template(src: str) -> str:
    """Replace Go template directives with YAML-safe placeholders."""
    # Control-flow and whitespace directives vanish entirely; they contribute
    # no YAML nodes.
    src = re.sub(
        r"\{\{-?\s*(if|else|else if|end|with|range|define|template|include|toYaml|nindent|indent)\b[^}]*\}\}",
        "",
        src,
    )
    # Remaining value substitutions become a scalar placeholder.
    src = re.sub(r"\{\{[^}]*\}\}", "PLACEHOLDER", src)
    return src


def check(path: str):
    src = open(path, encoding="utf-8").read()
    helm = is_helm_template(path)
    text = strip_go_template(src) if helm else src
    try:
        docs = [d for d in yaml.safe_load_all(text) if d]
        return None, len(docs), helm
    except yaml.YAMLError as exc:
        return exc, 0, helm


def main() -> int:
    patterns = [
        "deploy/gitops-manifests/**/*.yaml",
        "deploy/gitops-manifests/**/*.yml",
        "platform/ci-templates/**/*.yml",
        "services/**/*.yaml",
    ]
    files = []
    for pattern in patterns:
        files.extend(glob.glob(pattern, recursive=True))
    files = sorted(set(files))

    if not files:
        print("no YAML files found")
        return 1

    failures = 0
    for path in files:
        err, ndocs, helm = check(path)
        tag = " [helm]" if helm else ""
        if err:
            failures += 1
            print("  FAIL  {}{}".format(path, tag))
            print("        {}".format(str(err).replace("\n", "\n        ")[:400]))
        else:
            print("  OK    {}{} ({} doc{})".format(
                path, tag, ndocs, "" if ndocs == 1 else "s"))

    print()
    print("{} file(s), {} failure(s)".format(len(files), failures))
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
