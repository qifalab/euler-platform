#!/usr/bin/env python3
"""SQLite snapshots, with verification and an offline, recoverable restore.

Backups use SQLite's backup API, so committed WAL data is included. Never copy
only the live .db file. Stop app-cloud before restore; the required flag records
that operator precondition, it cannot stop a process in a different container.
"""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import sqlite3
import sys
import tempfile
from datetime import datetime, timezone


def connect_existing(path: Path) -> sqlite3.Connection:
    if not path.is_file() or path.is_symlink():
        raise ValueError("database must be an existing regular file, not a symlink")
    return sqlite3.connect(path.resolve().as_uri() + "?mode=ro", uri=True, timeout=10)


def verify(path: Path) -> None:
    db = connect_existing(path)
    try:
        if db.execute("PRAGMA integrity_check").fetchall() != [("ok",)]:
            raise ValueError("SQLite integrity check failed")
        if db.execute("PRAGMA foreign_key_check").fetchone() is not None:
            raise ValueError("SQLite foreign key check failed")
    finally:
        db.close()


def sync_directory(path: Path) -> None:
    fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def snapshot(source: Path, destination: Path) -> None:
    if source.resolve() == destination.resolve():
        raise ValueError("source and destination must differ")
    if destination.exists() or destination.is_symlink():
        raise ValueError("snapshot already exists; choose a new filename")
    if not destination.parent.is_dir():
        raise ValueError("destination directory must already exist")
    source_db = connect_existing(source)
    temp_name: str | None = None
    try:
        fd, temp_name = tempfile.mkstemp(prefix=".sqlite-snapshot-", dir=destination.parent)
        os.close(fd)  # mkstemp creates mode 0600; no credentials are logged.
        target_db = sqlite3.connect(temp_name)
        try:
            source_db.backup(target_db)
            target_db.execute("PRAGMA journal_mode=DELETE")
            target_db.commit()
        finally:
            target_db.close()
        verify(Path(temp_name))
        with open(temp_name, "rb") as handle:
            os.fsync(handle.fileno())
        # An atomic no-clobber publication also protects against parallel backups.
        os.link(temp_name, destination)
        sync_directory(destination.parent)
    finally:
        source_db.close()
        if temp_name is not None:
            Path(temp_name).unlink(missing_ok=True)


def restore(source: Path, destination: Path, service_stopped: bool) -> Path | None:
    if not service_stopped:
        raise ValueError("stop app-cloud first and pass --service-stopped")
    if source.resolve() == destination.resolve() or destination.is_symlink():
        raise ValueError("restore target must differ from the snapshot and cannot be a symlink")
    verify(source)
    if not destination.parent.is_dir():
        raise ValueError("destination directory must already exist")

    rollback: Path | None = None
    if destination.exists():
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
        rollback = destination.with_name(destination.name + ".pre-restore-" + stamp)
        snapshot(destination, rollback)
        # Recover/checkpoint an uncleanly stopped database before replacement.
        # Do not delete its WAL: it may contain the most recent committed data.
        db = sqlite3.connect(destination, timeout=10)
        try:
            result = db.execute("PRAGMA wal_checkpoint(TRUNCATE)").fetchone()
            if result and result[0] != 0:
                raise ValueError("database is busy; confirm app-cloud is stopped")
        finally:
            db.close()
    if any(Path(str(destination) + suffix).exists() for suffix in ("-wal", "-shm")):
        raise ValueError("database has active WAL files; confirm all writers are stopped")

    fd, staged_name = tempfile.mkstemp(prefix=".sqlite-restore-", dir=destination.parent)
    os.close(fd)
    staged = Path(staged_name)
    staged.unlink()
    try:
        snapshot(source, staged)
        os.replace(staged, destination)
        sync_directory(destination.parent)
    finally:
        staged.unlink(missing_ok=True)
    return rollback


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    backup = commands.add_parser("backup", help="create a verified, private standalone snapshot")
    backup.add_argument("database", type=Path)
    backup.add_argument("snapshot", type=Path)
    check = commands.add_parser("verify", help="verify SQLite integrity and foreign keys")
    check.add_argument("snapshot", type=Path)
    recover = commands.add_parser("restore", help="restore offline; preserve the old database first")
    recover.add_argument("snapshot", type=Path)
    recover.add_argument("database", type=Path)
    recover.add_argument("--service-stopped", action="store_true")
    args = parser.parse_args()
    try:
        if args.command == "backup":
            snapshot(args.database, args.snapshot)
            print(f"Verified snapshot created: {args.snapshot}")
        elif args.command == "verify":
            verify(args.snapshot)
            print("SQLite integrity and foreign key checks passed")
        else:
            rollback = restore(args.snapshot, args.database, args.service_stopped)
            print(f"Database restored: {args.database}")
            if rollback:
                print(f"Previous database preserved: {rollback}")
    except (OSError, sqlite3.Error, ValueError) as exc:
        print(f"Snapshot operation failed: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
