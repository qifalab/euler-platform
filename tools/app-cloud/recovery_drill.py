#!/usr/bin/env python3
"""Restore a backup into a new isolated directory and inspect every SQLite table.

This tests archive recovery, not production dependencies or external SQL/S3
restoration. It never starts Euler or sends messages from a restored outbox.
"""
import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import sqlite3
import sys
import zipfile
from platform_backup import restore


def drill(archive: Path, target: Path, allow_incomplete: bool = False) -> dict:
    manifest = restore(archive, target, True, not allow_incomplete)
    databases = []
    for entry in manifest["files"]:
        if entry["kind"] != "sqlite":
            continue
        path = target / entry["path"]
        db = sqlite3.connect(path.as_uri() + "?mode=ro", uri=True)
        try:
            tables = []
            for (name,) in db.execute("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name").fetchall():
                quoted = '"' + name.replace('"', '""') + '"'
                tables.append({"table": name, "rows": db.execute("SELECT count(*) FROM " + quoted).fetchone()[0]})
            databases.append({"path": entry["path"], "tables": tables})
        finally:
            db.close()
    return {"performedAt": datetime.now(timezone.utc).isoformat(), "archiveRecovery": "passed", "release": manifest["release"],
            "databases": databases, "externalResources": [{"kind": r["kind"], "id": r["id"], "artifactStatus": r["status"], "actualRestore": "pending"} for r in manifest["externalResources"]],
            "productionRestoreAccepted": False, "serviceStarted": False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archive", type=Path)
    parser.add_argument("destination", type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--allow-incomplete-external", action="store_true")
    args = parser.parse_args()
    try:
        report = drill(args.archive, Path(os.path.abspath(args.destination)), args.allow_incomplete_external)
        fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as output:
            json.dump(report, output, indent=2, ensure_ascii=False)
            output.write("\n")
        print(json.dumps({"archiveRecovery": "passed", "inspectedDatabases": len(report["databases"]), "productionRestoreAccepted": False}))
        return 0
    except (OSError, ValueError, KeyError, TypeError, sqlite3.Error, zipfile.BadZipFile) as exc:
        print(f"Recovery drill failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
