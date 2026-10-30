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

    def status(self, env, ident, production=True):
        return next(check["status"] for check in config_checks(env, production) if check["id"] == ident)

    def test_identity_settings_the_server_rejects_are_not_passed(self):
        # identity.Config.validate always needs a client secret and knows only two methods;
        # a rejected config disables login while the server keeps running.
        env = self.configuration()
        env.update(EULER_OIDC_TOKEN_AUTH_METHOD="none", EULER_OIDC_CLIENT_SECRET="")
        self.assertEqual(self.status(env, "identity-client"), "failed")
        env = self.configuration()
        env["EULER_OIDC_TOKEN_AUTH_METHOD"] = ""  # compose and the server both default to client_secret_basic
        self.assertEqual(self.status(env, "identity-client"), "passed")
        env = self.configuration()
        env["EULER_OAUTH2_SUBJECT_CLAIM"] = "id"  # "OIDC requires standard sub claims"
        self.assertEqual(self.status(env, "identity-mode"), "failed")
        env["EULER_IDENTITY_MODE"] = "oauth2_userinfo"
        self.assertEqual(self.status(env, "identity-mode"), "passed")
        env = self.configuration()
        env["EULER_IDENTITY_MODE"] = ""
        self.assertEqual(self.status(env, "identity-mode"), "passed")

    def test_values_that_stop_server_startup_are_not_passed(self):
        env = self.configuration()
        for value in ("172.18.0.2", "172.18.0.0/255.255.0.0"):  # netip.ParsePrefix refuses both
            env["EULER_WEAUTH_TRUSTED_PROXIES"] = value
            self.assertEqual(self.status(env, "trusted-proxies"), "failed", value)
        env["EULER_WEAUTH_TRUSTED_PROXIES"] = "172.18.0.0/16, fd00::/8"
        self.assertEqual(self.status(env, "trusted-proxies"), "passed")
        for token in (" " + "x" * 40 + " ", "x" * 4097):  # cmd/server: 32-4096 bytes, no surrounding space
            env["EULER_OPERATIONS_TOKEN"] = token
            self.assertEqual(self.status(env, "operations-token"), "failed", len(token))
        env["EULER_OPERATIONS_TOKEN"] = "x" * 40
        self.assertEqual(self.status(env, "operations-token"), "passed")

    def test_loopback_flag_is_read_like_go_parse_bool(self):
        env = self.configuration()
        for value in ("1", "t", "T", "True"):
            env["EULER_ALLOW_INSECURE_LOOPBACK"] = value
            self.assertEqual(self.status(env, "loopback-policy"), "failed", value)
        env["EULER_ALLOW_INSECURE_LOOPBACK"] = "yes"  # not a Go boolean: login is disabled
        self.assertEqual(self.status(env, "loopback-policy", production=False), "failed")
        preview = self.configuration()
        preview.update(EULER_PUBLIC_URL="http://localhost:8080", EULER_OIDC_ISSUER="http://127.0.0.1:9000",
                       EULER_ALLOW_INSECURE_LOOPBACK="false")
        self.assertEqual(self.status(preview, "public-origin", production=False), "failed")
        self.assertEqual(self.status(preview, "identity-provider", production=False), "failed")
        preview["EULER_ALLOW_INSECURE_LOOPBACK"] = "1"
        self.assertEqual(self.status(preview, "public-origin", production=False), "passed")
        self.assertEqual(self.status(preview, "identity-provider", production=False), "passed")


if __name__ == "__main__":
    unittest.main()
