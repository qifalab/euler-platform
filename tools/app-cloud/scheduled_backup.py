#!/usr/bin/env python3
"""Run one coordinated Compose backup. Schedule this with cron/systemd.

Optional hooks are administrator-owned executables, not shell command strings.
The freeze hook must stop direct SQL/S3 writers; capture writes manifest.json and
its artifacts; thaw resumes those writers. Euler is restarted even on failure.
"""
import argparse
from datetime import datetime, timezone
import fcntl
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import uuid


def run_backup(compose_dir: Path, *, external_dir: Path | None = None, freeze_hook: Path | None = None,
               capture_hook: Path | None = None, thaw_hook: Path | None = None,
               allow_incomplete: bool = False, release: str = "unspecified") -> str:
    compose_dir = compose_dir.resolve(strict=True)
    hooks = [freeze_hook, capture_hook, thaw_hook]
    if any(hooks) and (not all(hooks) or external_dir is None):
        raise ValueError("external backup requires all three hooks and a fresh external directory")
    if external_dir is not None and not all(hooks):
        raise ValueError("scheduled external capture requires freeze/capture/thaw hooks, not stale artifacts")
    for hook in hooks:
        if hook and (not hook.is_absolute() or hook.is_symlink() or not hook.is_file() or not os.access(hook, os.X_OK)):
            raise ValueError("hooks must be absolute executable regular files")
    if external_dir is not None:
        external_dir = Path(os.path.abspath(external_dir))
        if ":" in str(external_dir) or external_dir.exists() or external_dir.is_symlink():
            raise ValueError("external capture directory must be a fresh absolute mount path")
        external_dir.mkdir(parents=False, mode=0o700)
    compose = ["docker", "compose", "--env-file", ".env", "-f", "compose.yaml", "--profile", "maintenance"]
    def command(arguments):
        result = subprocess.run(arguments, cwd=compose_dir, check=True, capture_output=True, text=True)
        return result.stdout.strip()
    lock_path = compose_dir / ".euler-backup.lock"
    lock_fd = os.open(lock_path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(lock_fd, "w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        was_running = bool(command(compose + ["ps", "--status", "running", "-q", "app-cloud"]))
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
        archive = f"/backups/euler-{stamp}.zip"
        maintenance_name = "euler-backup-" + uuid.uuid4().hex
        frozen = False
        try:
            if freeze_hook:
                frozen = True  # Even a partially failed freeze must run the matching thaw.
                command([str(freeze_hook), str(external_dir)])
            if was_running:
                command(compose + ["stop", "app-cloud"])
            if command(compose + ["ps", "--status", "running", "-q", "app-cloud"]):
                raise ValueError("app-cloud is still running; backup refused")
            if capture_hook:
                command([str(capture_hook), str(external_dir)])
            options = ["run", "--rm", "--no-deps", "-T", "--name", maintenance_name]
            if external_dir:
                options += ["--volume", f"{external_dir}:/external:ro"]
            arguments = ["backup", "--data-dir", "/data", "--output", archive, "--service-stopped", "--release", release]
            if external_dir:
                arguments += ["--external-inventory", "/external/manifest.json"]
            if allow_incomplete:
                arguments += ["--allow-incomplete-external"]
            command(compose + options + ["maintenance"] + arguments)
            verify_args = ["verify", archive]
            if allow_incomplete:
                verify_args += ["--allow-incomplete-external"]
            command(compose + ["run", "--rm", "--no-deps", "-T", "--name", maintenance_name, "maintenance"] + verify_args)
        finally:
            try:
                # If the foreground docker client was interrupted, the daemon may
                # still run its container. Release its DB handles before restart.
                subprocess.run(["docker", "rm", "--force", maintenance_name], cwd=compose_dir, capture_output=True, check=False)
            finally:
                try:
                    if was_running:
                        command(compose + ["start", "app-cloud"])
                finally:
                    if frozen:
                        command([str(thaw_hook), str(external_dir)])
        return archive


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--compose-dir", type=Path, default=Path("deploy/app-cloud"))
    parser.add_argument("--external-dir", type=Path)
    for name in ("freeze", "capture", "thaw"):
        parser.add_argument(f"--{name}-hook", type=Path)
    parser.add_argument("--release", default="unspecified")
    parser.add_argument("--allow-incomplete-external", action="store_true")
    args = parser.parse_args()
    def interrupted(signum, frame):
        raise InterruptedError("backup interrupted; restoring prior service state")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    try:
        archive = run_backup(args.compose_dir, external_dir=args.external_dir, freeze_hook=args.freeze_hook,
                             capture_hook=args.capture_hook, thaw_hook=args.thaw_hook,
                             release=args.release, allow_incomplete=args.allow_incomplete_external)
        print(json.dumps({"backup": archive, "verified": True, "offsiteCopyRequired": True, "productionRestoreAccepted": False}))
        return 0
    except (OSError, ValueError, subprocess.CalledProcessError) as exc:
        # Hooks can emit DSNs/passwords. Their stdout/stderr are deliberately not echoed.
        print(f"Scheduled backup failed ({type(exc).__name__}); inspect protected operator logs and service status.", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
