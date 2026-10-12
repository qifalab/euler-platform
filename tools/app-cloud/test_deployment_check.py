import base64
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from deployment_check import config_checks, read_environment, acceptance_checks


class DeploymentCheckTests(unittest.TestCase):
    def configuration(self):
        return {"EULER_ENCRYPTION_KEY": base64.b64encode(b"k" * 32).decode(),
                "EULER_PUBLIC_URL": "https://cloud.example.test", "EULER_OIDC_ISSUER": "https://id.example.test",
                "EULER_OIDC_CLIENT_ID": "euler", "EULER_OIDC_CLIENT_SECRET": "secret-not-for-logs",
                "EULER_PLATFORM_ADMIN_IDENTITIES": '[{"provider":"https://id.example.test","subject":"operator"}]'}

    def test_static_checks_do_not_claim_production_acceptance_or_print_secrets(self):
        checks = config_checks(self.configuration(), True)
        self.assertFalse(any(check["status"] == "failed" for check in checks))
        self.assertTrue(any(check["id"] == "real-identity-login" and check["status"] == "pending" for check in checks))
        self.assertNotIn("secret-not-for-logs", json.dumps(checks))

    def test_http_production_and_wildcard_proxy_are_refused(self):
        env = self.configuration()
        env.update(EULER_PUBLIC_URL="http://localhost:8080", EULER_ALLOW_INSECURE_LOOPBACK="true", EULER_WEAUTH_TRUSTED_PROXIES="0.0.0.0/0")
        failed = {check["id"] for check in config_checks(env, True) if check["status"] == "failed"}
        self.assertTrue({"public-origin", "loopback-policy", "trusted-proxies"}.issubset(failed))

    def test_dotenv_is_literal_and_never_evaluated(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / "settings"
            path.write_text("# comments\nEULER_PUBLIC_URL=https://cloud.example.test\nJSON='[{\"id\":\"value\"}]'\n")
            self.assertEqual(read_environment(path)["JSON"], '[{"id":"value"}]')
            path.write_text("VALUE=$(touch /tmp/should-never-run)\n")
            with self.assertRaises(ValueError):
                read_environment(path)

    def test_private_material_endpoint_requires_matching_explicit_credentials(self):
        env = self.configuration()
        env.update(EULER_TRUST_MATERIAL_S3_BUCKET="private-trust", EULER_TRUST_MATERIAL_S3_ENDPOINT="other.example.test",
                   EULER_STORAGE_S3_ENDPOINT="storage.example.test", EULER_STORAGE_S3_ACCESS_KEY="storage-key", EULER_STORAGE_S3_SECRET_KEY="storage-secret")
        def status():
            return next(check["status"] for check in config_checks(env, True) if check["id"] == "trust-private-material-storage")
        self.assertEqual(status(), "failed")
        env.update(EULER_TRUST_MATERIAL_S3_ACCESS_KEY="trust-key", EULER_TRUST_MATERIAL_S3_SECRET_KEY="trust-secret")
        self.assertEqual(status(), "passed")
        env["EULER_TRUST_MATERIAL_S3_SECURE"] = "false"
        self.assertEqual(status(), "failed")

    def test_acceptance_fails_open_access_and_preserves_manual_pending(self):
        def response(base, path):
            return 200, {}, b'{"authenticated":false}'
        with patch("deployment_check.http_probe", side_effect=response):
            checks = acceptance_checks("https://cloud.example.test")
        self.assertEqual(next(c for c in checks if c["id"] == "unauthenticated-projects")["status"], "failed")
        self.assertEqual(next(c for c in checks if c["id"] == "real-smtp-delivery")["status"], "pending")
        with self.assertRaises(ValueError):
            acceptance_checks("http://example.test")


if __name__ == "__main__":
    unittest.main()
