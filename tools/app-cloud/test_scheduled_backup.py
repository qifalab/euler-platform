from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
from scheduled_backup import run_backup


class ScheduledBackupTests(unittest.TestCase):
    def test_backup_failure_still_restarts_previously_running_service(self):
        calls = []
        queries = 0
        def command(args, **kwargs):
            nonlocal queries
            calls.append(args)
            if "ps" in args:
                queries += 1
                return subprocess.CompletedProcess(args, 0, "container" if queries == 1 else "", "")
            if "backup" in args:
                raise subprocess.CalledProcessError(1, args, stderr="secret must not be echoed")
            return subprocess.CompletedProcess(args, 0, "", "")
        with tempfile.TemporaryDirectory() as root, patch("scheduled_backup.subprocess.run", side_effect=command):
            with self.assertRaises(subprocess.CalledProcessError):
                run_backup(Path(root))
        self.assertIn(["stop", "app-cloud"], [call[-2:] for call in calls])
        self.assertEqual(calls[-1][-2:], ["start", "app-cloud"])

    def test_already_stopped_service_is_not_started(self):
        calls = []
        def command(args, **kwargs):
            calls.append(args)
            return subprocess.CompletedProcess(args, 0, "", "")
        with tempfile.TemporaryDirectory() as root, patch("scheduled_backup.subprocess.run", side_effect=command):
            archive = run_backup(Path(root))
        self.assertTrue(archive.endswith(".zip"))
        self.assertFalse(any("start" in call for call in calls))
        self.assertTrue(any("verify" in call for call in calls))

    def test_stale_external_directory_and_unpaired_hooks_refused(self):
        with tempfile.TemporaryDirectory() as root:
            with self.assertRaises(ValueError):
                run_backup(Path(root), external_dir=Path(root))
            with self.assertRaises(ValueError):
                run_backup(Path(root), freeze_hook=Path("/not-used"))

    def test_maintenance_runs_stage_on_the_backup_volume_not_tmpfs(self):
        # verify unpacks the whole archive under TMPDIR; the container's /tmp is RAM-backed tmpfs.
        calls = []
        def command(args, **kwargs):
            calls.append(args)
            return subprocess.CompletedProcess(args, 0, "", "")
        with tempfile.TemporaryDirectory() as root, patch("scheduled_backup.subprocess.run", side_effect=command):
            run_backup(Path(root))
        runs = [call for call in calls if "run" in call]
        self.assertEqual([call[call.index("maintenance", call.index("run")) + 1] for call in runs], ["backup", "verify"])
        for call in runs:
            options = call[call.index("run"):call.index("maintenance", call.index("run"))]
            self.assertIn("TMPDIR=/backups", options)
            self.assertEqual(options[options.index("TMPDIR=/backups") - 1], "--env")


if __name__ == "__main__":
    unittest.main()
