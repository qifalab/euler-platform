#!/usr/bin/env python3
"""Offline Euler data backup, verification and no-clobber recovery (stdlib only).

All app data must be below --data-dir. Stop every writer, including direct SQL/S3
clients, before taking a coordinated recovery point. SQLite write reservations
and file-change detection are additional checks, not a distributed freeze.
"""
from __future__ import annotations

import argparse
import base64
from contextlib import ExitStack, closing
from datetime import datetime, timezone
import hashlib
import hmac
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import sqlite3
import stat
import sys
import tempfile
import zipfile

from sqlite_snapshot import snapshot, verify as verify_sqlite, sync_directory

FORMAT = "euler-platform-backup/v1"
MAX_MANIFEST = 4 * 1024 * 1024
MAX_ENTRIES = 100000


def safe_path(path: Path) -> Path:
    """Reject links in every existing component, including destination parents."""
    path = Path(os.path.abspath(path))
    for component in [path, *path.parents]:
        if component.is_symlink():
            raise ValueError("symbolic links are not accepted")
    return path


def relative_name(value: str) -> str:
    if not isinstance(value, str) or not value or "\\" in value or "\x00" in value:
        raise ValueError("invalid archive path")
    parts = value.split("/")
    if any(part in ("", ".", "..") for part in parts) or PurePosixPath(value).is_absolute():
        raise ValueError("unsafe archive path")
    if ":" in parts[0]:
        raise ValueError("drive-qualified archive path")
    return value


def digest(path: Path) -> str:
    with path.open("rb") as handle:
        return hashlib.file_digest(handle, "sha256").hexdigest()


def encryption_key(key: str | None = None) -> bytes:
    try:
        value = base64.b64decode(key if key is not None else os.environ.get("EULER_ENCRYPTION_KEY", ""), validate=True)
    except ValueError as exc:
        raise ValueError("EULER_ENCRYPTION_KEY must be base64 encoded") from exc
    if len(value) != 32:
        raise ValueError("EULER_ENCRYPTION_KEY must decode to 32 bytes")
    return value


def key_fingerprint(key: str | None = None) -> str:
    return hashlib.sha256(b"euler-backup-key/v1\x00" + encryption_key(key)).hexdigest()


def manifest_signature(manifest: dict) -> str:
    # Authenticate the manifest without including the master key or reusing it directly.
    signing_key = hmac.new(encryption_key(), b"euler-backup-manifest/v1", hashlib.sha256).digest()
    unsigned = {key: value for key, value in manifest.items() if key != "manifestHMAC"}
    canonical = json.dumps(unsigned, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()
    return hmac.new(signing_key, canonical, hashlib.sha256).hexdigest()


def inventory(root: Path) -> dict[str, tuple[int, int, int, int]]:
    files = {}
    for directory, dirs, names in os.walk(root, followlinks=False):
        for name in sorted(dirs + names):
            path = Path(directory) / name
            mode = path.lstat().st_mode
            if stat.S_ISLNK(mode) or not (stat.S_ISDIR(mode) or stat.S_ISREG(mode)):
                raise ValueError("data contains a link or non-regular file")
            if stat.S_ISREG(mode):
                st = path.stat()
                files[relative_name(path.relative_to(root).as_posix())] = (st.st_dev, st.st_ino, st.st_size, st.st_mtime_ns)
    return files


def sqlite_files(root: Path, names: dict) -> list[str]:
    result = []
    for name in names:
        path = root / name
        with path.open("rb") as handle:
            header = handle.read(16)
        if header == b"SQLite format 3\x00" or path.suffix.lower() in (".db", ".sqlite", ".sqlite3"):
            verify_sqlite(path)  # A broken SQLite file must never become an opaque file backup.
            result.append(name)
    return sorted(result)


def reject_open_files(root: Path) -> None:
    """Linux same-PID-namespace guard; containers still require a verified stop."""
    proc = Path("/proc")
    if not proc.is_dir():
        return
    own_processes = {str(os.getpid()), (proc / "self").resolve().name}
    for process in proc.iterdir():
        if not process.name.isdigit() or process.name in own_processes:
            continue
        try:
            descriptors = list((process / "fd").iterdir())
        except (PermissionError, FileNotFoundError, ProcessLookupError):
            continue
        for descriptor in descriptors:
            try:
                target = Path(os.readlink(descriptor))
                if target.is_relative_to(root):
                    raise ValueError("another visible process has app data open; stop every writer first")
            except (PermissionError, FileNotFoundError, ProcessLookupError):
                continue


def external_inventory(database: Path) -> list[dict]:
    """Read identifiers only: credentials are never copied into the manifest."""
    with closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True)) as db:
        db.execute("BEGIN")
        tables = {row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table'")}
        resources = []
        if "database_instances" in tables:
            for ident, engine, name, user in db.execute("SELECT id,type,db_name,db_user FROM database_instances ORDER BY id"):
                resources.append({"kind": engine, "id": ident, "name": name, "user": user})
        if "storage_buckets" in tables:
            for ident, name in db.execute("SELECT id,name FROM storage_buckets ORDER BY id"):
                resources.append({"kind": "s3", "id": ident, "name": name})
        if "trust_material_objects" in tables:
            # Trust's private buckets are deliberately absent from storage_buckets.
            # Include orphan ledger entries too: deletion may still be pending.
            # Bucket-level identifiers expose neither material IDs nor object keys.
            for bucket, count in db.execute("SELECT bucket,count(*) FROM trust_material_objects GROUP BY bucket ORDER BY bucket"):
                if not isinstance(bucket, str) or not bucket:
                    raise ValueError("Trust material ledger contains an invalid bucket")
                ident = "trust-materials:" + hashlib.sha256(bucket.encode("utf-8")).hexdigest()
                resources.append({"kind": "s3", "id": ident, "name": bucket,
                                  "scope": "trust-private-materials", "objectCount": count})
        return resources


def copy_external(spec_path: Path | None, resources: list[dict], staged: Path, require: bool) -> list[dict]:
    supplied = {}
    if spec_path:
        spec_path = safe_path(spec_path)
        if spec_path.stat().st_size > MAX_MANIFEST:
            raise ValueError("external inventory is too large")
        spec = json.loads(spec_path.read_text())
        if spec.get("schemaVersion") != 1 or not isinstance(spec.get("resources"), list):
            raise ValueError("invalid external inventory schema")
        for resource in spec["resources"]:
            key = (resource.get("kind"), resource.get("id"))
            if key in supplied:
                raise ValueError("duplicate external resource")
            supplied[key] = resource
    known = {(r["kind"], r["id"]) for r in resources}
    if set(supplied) - known:
        raise ValueError("external inventory includes an unknown resource")
    output = []
    for resource in resources:
        entry = dict(resource, status="missing", artifacts=[])
        provided = supplied.get((resource["kind"], resource["id"]))
        if provided:
            artifacts = provided.get("artifacts", [])
            purposes = {a.get("purpose") for a in artifacts}
            if not {"data", "configuration"}.issubset(purposes):
                raise ValueError("each external resource requires data and configuration artifacts")
            captured = provided.get("capturedAt", "")
            if not captured or not provided.get("restoreProcedure"):
                raise ValueError("external resource requires capturedAt and restoreProcedure")
            datetime.fromisoformat(captured.replace("Z", "+00:00"))
            for index, artifact in enumerate(artifacts):
                source = safe_path(spec_path.parent / relative_name(artifact["path"]))
                if not source.is_file() or digest(source) != artifact.get("sha256"):
                    raise ValueError("external artifact missing or checksum mismatch")
                # Resource IDs are metadata, never path components.
                target = staged / "external" / str(len(output)) / f"{index}-{source.name}"
                target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                shutil.copyfile(source, target)
                target.chmod(0o600)
                if digest(target) != artifact["sha256"]:
                    raise ValueError("external artifact changed while being copied")
                entry["artifacts"].append({"path": target.relative_to(staged).as_posix(), "purpose": artifact["purpose"]})
            entry.update(status="artifacts-verified", capturedAt=captured, restoreProcedure=provided["restoreProcedure"])
        if require and entry["status"] == "missing":
            raise ValueError("external data is missing; supply --external-inventory or use --allow-incomplete-external")
        output.append(entry)
    return output


def backup(data_dir: Path, destination: Path, service_stopped: bool, external_spec: Path | None = None,
           require_external: bool = True, main_database: str = "app-cloud.db", release: str = "unspecified") -> dict:
    if not service_stopped:
        raise ValueError("stop app-cloud and other data writers, then pass --service-stopped")
    fingerprint = key_fingerprint()
    root, destination = safe_path(data_dir), safe_path(destination)
    if not root.is_dir() or not destination.parent.is_dir():
        raise ValueError("source and destination parent directories must exist")
    if destination.exists() or destination.is_relative_to(root):
        raise ValueError("backup must be a new file outside the data directory")
    main_database = relative_name(main_database)
    reject_open_files(root)
    initial = inventory(root)
    databases = sqlite_files(root, initial)
    if main_database not in databases:
        raise ValueError("main database not found in data directory")
    # Hold write reservations on all SQLite DBs for the entire capture, not one by one.
    with ExitStack() as stack:
        for name in databases:
            db = sqlite3.connect((root / name).as_uri() + "?mode=rw", uri=True, timeout=0)
            stack.callback(db.close)
            try:
                db.execute("BEGIN IMMEDIATE")
            except sqlite3.OperationalError as exc:
                raise ValueError("database has an active writer; stop the service") from exc
        baseline = inventory(root)
        with tempfile.TemporaryDirectory(prefix=".euler-backup-", dir=destination.parent) as temp:
            staged = Path(temp)
            sidecars = {name + suffix for name in databases for suffix in ("-wal", "-shm", "-journal")}
            for name in sorted(baseline):
                if name in sidecars:
                    continue  # SQLite backup API incorporates committed WAL, no sidecar is restored.
                target = staged / "data" / name
                target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                if name in databases:
                    snapshot(root / name, target)
                else:
                    shutil.copyfile(root / name, target)
                    target.chmod(0o600)
            external = copy_external(external_spec, external_inventory(staged / "data" / main_database), staged, require_external)
            # Ignore only SHM mtime: our readonly backup connection can update SQLite's index.
            sqlite_indexes = {name + "-shm" for name in databases}
            def stable(inv):
                return {k: v for k, v in inv.items() if k not in sqlite_indexes}
            if stable(baseline) != stable(inventory(root)):
                raise ValueError("app data changed during capture; all writers must be stopped")
            manifest = {"format": FORMAT, "createdAt": datetime.now(timezone.utc).isoformat(),
                        "release": release, "keyFingerprint": fingerprint, "mainDatabase": main_database,
                        "consistency": "operator-confirmed-offline", "externalResources": external,
                        "externalCoverage": "incomplete" if any(r["status"] == "missing" for r in external) else "artifacts-included",
                        "files": []}
            for name in sorted(inventory(staged)):
                path = staged / name
                manifest["files"].append({"path": name, "size": path.stat().st_size, "sha256": digest(path),
                                          "kind": "sqlite" if name.startswith("data/") and name[5:] in databases else "file"})
            manifest["manifestHMAC"] = manifest_signature(manifest)
            archive = staged / "archive.zip"
            with zipfile.ZipFile(archive, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=6) as output:
                for entry in manifest["files"]:
                    output.write(staged / entry["path"], entry["path"])
                output.writestr("manifest.json", json.dumps(manifest, ensure_ascii=False, indent=2))
            archive.chmod(0o600)
            with archive.open("rb") as handle:
                os.fsync(handle.fileno())
            os.link(archive, destination)  # Never overwrite an existing recovery point.
            sync_directory(destination.parent)
            return manifest


def extract_verified(archive: Path, target: Path, max_bytes: int, require_external: bool) -> dict:
    archive = safe_path(archive)
    with zipfile.ZipFile(archive) as bundle:
        entries = bundle.infolist()
        if len(entries) > MAX_ENTRIES or sum(e.file_size for e in entries) > max_bytes:
            raise ValueError("archive exceeds configured size/entry limit")
        names = [relative_name(e.filename) for e in entries]
        if len(set(names)) != len(names):
            raise ValueError("duplicate archive paths")
        for entry in entries:
            mode = entry.external_attr >> 16
            if entry.is_dir() or (stat.S_IFMT(mode) and not stat.S_ISREG(mode)):
                raise ValueError("archive contains directory/link/special entries")
        if "manifest.json" not in names or bundle.getinfo("manifest.json").file_size > MAX_MANIFEST:
            raise ValueError("manifest missing or too large")
        manifest = json.loads(bundle.read("manifest.json"))
        if manifest.get("format") != FORMAT or manifest.get("keyFingerprint") != key_fingerprint():
            raise ValueError("unsupported format or encryption key fingerprint mismatch")
        if not hmac.compare_digest(str(manifest.get("manifestHMAC", "")), manifest_signature(manifest)):
            raise ValueError("manifest authentication failed")
        files = manifest.get("files", [])
        declared = [relative_name(e["path"]) for e in files]
        if len(set(declared)) != len(declared) or set(names) != set(declared) | {"manifest.json"}:
            raise ValueError("manifest does not exactly match archive files")
        if any(not (name.startswith("data/") or name.startswith("external/")) for name in declared):
            raise ValueError("undeclared data root")
        sqlite_names = []
        for entry in files:
            name = entry["path"]
            info = bundle.getinfo(name)
            if info.file_size != entry["size"] or entry["kind"] not in ("file", "sqlite"):
                raise ValueError("invalid manifest entry")
            destination = target / name
            destination.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            with bundle.open(name) as source, destination.open("xb") as output:
                shutil.copyfileobj(source, output)
            destination.chmod(0o600)
            if digest(destination) != entry["sha256"]:
                raise ValueError("archive content checksum mismatch")
            if entry["kind"] == "sqlite":
                verify_sqlite(destination)
                sqlite_names.append(name)
        main = "data/" + relative_name(manifest["mainDatabase"])
        if main not in sqlite_names:
            raise ValueError("manifest lacks main SQLite database")
        resources = external_inventory(target / main)
        external = manifest.get("externalResources", [])
        if len({(r["kind"], r["id"]) for r in external}) != len(external):
            raise ValueError("duplicate external resource")
        if {(r["kind"], r["id"], r["name"]) for r in resources} != {(r["kind"], r["id"], r["name"]) for r in external}:
            raise ValueError("external inventory differs from actual control metadata")
        declared_resources = {(r["kind"], r["id"]): r for r in external}
        for expected in resources:
            actual = declared_resources[(expected["kind"], expected["id"])]
            if any(actual.get(key) != value for key, value in expected.items()):
                raise ValueError("external resource details differ from actual control metadata")
        for resource in external:
            artifacts = resource.get("artifacts", [])
            if resource.get("status") == "artifacts-verified":
                if not {"data", "configuration"}.issubset({a.get("purpose") for a in artifacts}):
                    raise ValueError("external artifacts are incomplete")
                if any(a["path"] not in declared or not a["path"].startswith("external/") for a in artifacts):
                    raise ValueError("external artifact missing from bundle")
            elif require_external or resource.get("status") != "missing":
                raise ValueError("external resource backup is incomplete")
        coverage = "incomplete" if any(r["status"] == "missing" for r in external) else "artifacts-included"
        if manifest.get("externalCoverage") != coverage:
            raise ValueError("external coverage does not match inventory")
        return manifest


def verify(archive: Path, require_external: bool = True, max_bytes: int = 64 * 1024**3) -> dict:
    with tempfile.TemporaryDirectory(prefix="euler-verify-") as temporary:
        return extract_verified(archive, Path(temporary), max_bytes, require_external)


def restore(archive: Path, destination: Path, service_stopped: bool, require_external: bool = True,
            max_bytes: int = 64 * 1024**3) -> dict:
    if not service_stopped:
        raise ValueError("stop app-cloud first and pass --service-stopped")
    destination = safe_path(destination)
    if destination.exists() or not destination.parent.is_dir():
        raise ValueError("restore requires a new destination in an existing parent directory")
    with tempfile.TemporaryDirectory(prefix=".euler-restore-", dir=destination.parent) as temp:
        staged = Path(temp)
        manifest = extract_verified(archive, staged, max_bytes, require_external)
        (staged / "manifest.json").write_text(json.dumps(manifest, ensure_ascii=False, indent=2))
        (staged / "manifest.json").chmod(0o600)
        # mkdir is the atomic no-clobber claim. Never merge with an old data tree.
        destination.mkdir(mode=0o700)
        try:
            for child in staged.iterdir():
                shutil.move(str(child), destination / child.name)
            for directory, _, filenames in os.walk(destination):
                for filename in filenames:
                    with (Path(directory) / filename).open("rb") as handle:
                        os.fsync(handle.fileno())
                sync_directory(Path(directory))
            sync_directory(destination.parent)
        except BaseException:
            shutil.rmtree(destination)
            raise
        return manifest


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    inspect = commands.add_parser("inventory", help="list actual external SQL/S3 resource identifiers without credentials")
    inspect.add_argument("database", type=Path)
    create = commands.add_parser("backup")
    create.add_argument("--data-dir", required=True, type=Path)
    create.add_argument("--output", required=True, type=Path)
    create.add_argument("--main-database", default="app-cloud.db")
    create.add_argument("--release", default="unspecified")
    create.add_argument("--external-inventory", type=Path)
    create.add_argument("--service-stopped", action="store_true")
    for name in ("verify", "restore"):
        command = commands.add_parser(name)
        command.add_argument("archive", type=Path)
        command.add_argument("--max-bytes", type=int, default=64 * 1024**3)
        if name == "restore":
            command.add_argument("destination", type=Path)
            command.add_argument("--service-stopped", action="store_true")
    for command in (create, *[commands.choices[n] for n in ("verify", "restore")]):
        command.add_argument("--allow-incomplete-external", action="store_true", help="explicit metadata-only recovery point; never a full external data backup")
    args = parser.parse_args()
    try:
        if args.command == "inventory":
            database = safe_path(args.database)
            verify_sqlite(database)
            resources = external_inventory(database)
            for resource in resources:
                resource.update(capturedAt="", restoreProcedure="", artifacts=[])
            print(json.dumps({"schemaVersion": 1, "resources": resources}, indent=2))
            return 0
        complete = not args.allow_incomplete_external
        if args.command == "backup":
            result = backup(args.data_dir, args.output, args.service_stopped, args.external_inventory, complete, args.main_database, args.release)
        elif args.command == "verify":
            result = verify(args.archive, complete, args.max_bytes)
        else:
            result = restore(args.archive, args.destination, args.service_stopped, complete, args.max_bytes)
        print(json.dumps({"operation": args.command, "verifiedFiles": len(result["files"]), "externalCoverage": result["externalCoverage"],
                          "release": result["release"], "productionRestoreAccepted": False}))
        return 0
    except (OSError, ValueError, KeyError, TypeError, sqlite3.Error, zipfile.BadZipFile) as exc:
        print(f"Backup operation failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
