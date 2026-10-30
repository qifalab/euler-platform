"""Deploy configuration must match what the backend and the operations docs rely on."""
from pathlib import Path
import re
import unittest

DEPLOY = Path(__file__).resolve().parents[2] / "deploy" / "app-cloud"


def nginx_size(text: str) -> int:
    value, unit = re.fullmatch(r"([0-9]+)([kKmMgG]?)", text).groups()
    return int(value) * {"": 1, "k": 1024, "m": 1024 ** 2, "g": 1024 ** 3}[unit.lower()]


def compose_service(name: str) -> list[str]:
    lines = (DEPLOY / "compose.yaml").read_text().splitlines()
    block = []
    for line in lines[lines.index(f"  {name}:") + 1:]:
        if line.strip() and not line.startswith("    "):
            break
        block.append(line)
    return block


class DeployConfigTests(unittest.TestCase):
    def test_public_storage_uploads_fit_the_backend_proxy_limit(self):
        # services/app-cloud/internal/apps/storage/public.go reads up to
        # EULER_STORAGE_MAX_PROXY_BYTES (default 100 MiB) + 1 MiB per upload.
        text = (DEPLOY / "nginx.conf").read_text()
        block = re.search(r"location /public/storage/ \{(.*?)\}", text, re.S)
        self.assertIsNotNone(block, "PUT /public/storage/... falls back to the server-wide 32m limit")
        size = re.search(r"client_max_body_size\s+(\S+);", block.group(1))
        self.assertIsNotNone(size)
        self.assertGreaterEqual(nginx_size(size.group(1)), 101 * 1024 ** 2)
        self.assertIn("access_log off;", block.group(1))  # object keys in URLs stay out of logs
        self.assertIn("proxy_pass http://app_cloud;", block.group(1))

    def test_app_cloud_stop_grace_covers_the_documented_shutdown(self):
        # cmd/server/main.go drains HTTP for up to 20 s after SIGTERM, then stops
        # workers and closes SQLite. Docker's default grace period is 10 s.
        values = [line.split(":", 1)[1].strip() for line in compose_service("app-cloud")
                  if line.strip().startswith("stop_grace_period:")]
        self.assertEqual(len(values), 1, "no stop_grace_period: Docker SIGKILLs after 10 s")
        amount, unit = re.fullmatch(r"([0-9]+)(s|m)", values[0]).groups()
        self.assertGreaterEqual(int(amount) * (60 if unit == "m" else 1), 25)

    def test_backup_unit_fails_rather_than_skips_without_a_deployment(self):
        # A failed Condition*= leaves the unit inactive (no failure, no alert).
        unit = (DEPLOY / "systemd" / "euler-backup.service").read_text()
        self.assertNotRegex(unit, r"(?m)^Condition")
        self.assertRegex(unit, r"(?m)^AssertPathExists=/opt/euler-platform/deploy/app-cloud/\.env$")


if __name__ == "__main__":
    unittest.main()
