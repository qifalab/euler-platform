import base64
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
import zipfile

from platform_backup import backup, restore, verify, digest, external_inventory, manifest_signature
from recovery_drill import drill


class PlatformBackupTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.data = self.root / "data"
        self.data.mkdir()
        self.archive = self.root / "backup.zip"
        self.key = base64.b64encode(b"k" * 32).decode()
        self.env = patch.dict(os.environ, {"EULER_ENCRYPTION_KEY": self.key})
        self.env.start()
        self.addCleanup(self.env.stop)
        self.create_db(self.data / "app-cloud.db", "platform record")
        for project in ("one", "two"):
            self.create_db(self.data / "apps" / "witshield" / project / "witshield.db", project)
        (self.data / "apps" / "proof.bin").write_bytes(b"real stored bytes\x00\xff")

    def create_db(self, path, value):
        path.parent.mkdir(parents=True, exist_ok=True)
        with sqlite3.connect(path) as db:
            db.execute("CREATE TABLE records(id INTEGER PRIMARY KEY,value TEXT)")
            db.execute("INSERT INTO records VALUES(1,?)", (value,))
        db.close()

    def create(self, **kwargs):
        return backup(self.data, self.archive, True, **kwargs)

    def rewrite(self, change):
        with zipfile.ZipFile(self.archive) as original:
            entries = {entry.filename: original.read(entry) for entry in original.infolist()}
        change(entries)
        replacement = self.root / "changed.zip"
        with zipfile.ZipFile(replacement, "w") as output:
            for name, data in entries.items():
                output.writestr(name, data)
        return replacement

    def add_external(self):
        with sqlite3.connect(self.data / "app-cloud.db") as db:
            db.executescript("CREATE TABLE database_instances(id TEXT,type TEXT,db_name TEXT,db_user TEXT);"
                             "INSERT INTO database_instances VALUES('db-1','postgres','project_db','project_user');"
                             "CREATE TABLE storage_buckets(id TEXT,name TEXT);"
                             "INSERT INTO storage_buckets VALUES('bucket-1','project-bucket');")

    def add_trust_material_ledger(self):
        with sqlite3.connect(self.data / "app-cloud.db") as db:
            db.executescript("CREATE TABLE trust_materials(id TEXT PRIMARY KEY,body BLOB);"
                             "INSERT INTO trust_materials VALUES('material-active',X'010203');"
                             "CREATE TABLE trust_material_objects(material_id TEXT PRIMARY KEY,bucket TEXT NOT NULL,object_key TEXT NOT NULL,created_at TEXT NOT NULL);"
                             "INSERT INTO trust_material_objects VALUES('material-active','private-current','materials/active/ciphertext','2026-09-27T10:00:00Z');"
                             "INSERT INTO trust_material_objects VALUES('orphan','private-current','materials/orphan/ciphertext','2026-09-26T10:00:00Z');"
                             "INSERT INTO trust_material_objects VALUES('old-orphan','private-previous','materials/old/ciphertext','2026-09-26T10:00:00Z');")
        db.close()

    def test_multi_database_and_files_restore_to_new_directory(self):
        result = self.create(release="test-commit")
        self.assertEqual(len(result["files"]), 4)
        self.assertEqual(sum(entry["kind"] == "sqlite" for entry in result["files"]), 3)
        self.assertNotIn(self.key, json.dumps(result))
        self.assertEqual(self.archive.stat().st_mode & 0o777, 0o600)
        verify(self.archive)
        restored = self.root / "recovered"
        restore(self.archive, restored, True)
        for path, expected in (("app-cloud.db", "platform record"), ("apps/witshield/one/witshield.db", "one"), ("apps/witshield/two/witshield.db", "two")):
            with sqlite3.connect(restored / "data" / path) as db:
                self.assertEqual(db.execute("SELECT value FROM records").fetchone(), (expected,))
        self.assertEqual((restored / "data/apps/proof.bin").read_bytes(), b"real stored bytes\x00\xff")
        with self.assertRaises(ValueError):
            restore(self.archive, restored, True)

    def test_wrong_key_cannot_verify_or_publish_restore(self):
        self.create()
        os.environ["EULER_ENCRYPTION_KEY"] = base64.b64encode(b"x" * 32).decode()
        with self.assertRaisesRegex(ValueError, "key fingerprint"):
            verify(self.archive)
        with self.assertRaises(ValueError):
            restore(self.archive, self.root / "recovered", True)
        self.assertFalse((self.root / "recovered").exists())

    def test_corruption_missing_extra_and_duplicate_files_are_rejected(self):
        self.create()
        for change in (lambda files: files.__setitem__("data/apps/proof.bin", b"corruption"),
                       lambda files: files.pop("data/app-cloud.db"),
                       lambda files: files.__setitem__("data/undeclared.txt", b"extra")):
            with self.subTest(change=change):
                with self.assertRaises(ValueError):
                    verify(self.rewrite(change))
        with zipfile.ZipFile(self.archive, "a") as archive:
            archive.writestr("data/apps/proof.bin", b"duplicate")
        with self.assertRaisesRegex(ValueError, "duplicate"):
            verify(self.archive)

    def test_zip_slip_and_symlink_archive_are_rejected(self):
        self.create()
        for name in ("../escape", "/absolute", "data/../escape", "data\\escape", "C:/escape"):
            with self.subTest(name=name):
                changed = self.rewrite(lambda files: files.__setitem__(name, b"bad"))
                with self.assertRaises(ValueError):
                    verify(changed)
        changed = self.root / "link.zip"
        with zipfile.ZipFile(changed, "w") as output:
            entry = zipfile.ZipInfo("data/link")
            entry.create_system = 3
            entry.external_attr = (stat.S_IFLNK | 0o777) << 16
            output.writestr(entry, "../../escape")
        with self.assertRaises(ValueError):
            verify(changed)
        self.assertFalse((self.root / "escape").exists())

    def test_links_live_writer_and_missing_stop_ack_are_rejected(self):
        with self.assertRaises(ValueError):
            backup(self.data, self.archive, False)
        link = self.data / "linked"
        link.symlink_to(self.root / "outside")
        with self.assertRaises(ValueError):
            self.create()
        link.unlink()
        with sqlite3.connect(self.data / "app-cloud.db") as writer:
            writer.execute("BEGIN IMMEDIATE")
            writer.execute("INSERT INTO records VALUES(2,'uncommitted')")
            with self.assertRaisesRegex(ValueError, "active writer"):
                self.create()
        self.assertFalse(self.archive.exists())

    def test_visible_other_process_is_refused(self):
        process = subprocess.Popen([sys.executable, "-c", "import sys; f=open(sys.argv[1],'rb'); print('ready',flush=True); sys.stdin.read()", str(self.data / "app-cloud.db")], stdin=subprocess.PIPE, stdout=subprocess.PIPE)
        try:
            process.stdout.readline()
            with self.assertRaisesRegex(ValueError, "another visible process"):
                self.create()
        finally:
            process.communicate(b"exit")

    def test_committed_wal_included_and_sidecars_not_restored(self):
        db = sqlite3.connect(self.data / "app-cloud.db")
        self.addCleanup(db.close)
        db.execute("PRAGMA journal_mode=WAL")
        db.execute("INSERT INTO records VALUES(2,'committed WAL')")
        db.commit()
        self.assertTrue((self.data / "app-cloud.db-wal").exists())
        self.create()
        restore(self.archive, self.root / "restored", True)
        with sqlite3.connect(self.root / "restored/data/app-cloud.db") as recovered:
            self.assertEqual(recovered.execute("SELECT count(*) FROM records").fetchone()[0], 2)
        self.assertFalse((self.root / "restored/data/app-cloud.db-wal").exists())

    def test_external_resources_cannot_be_silently_omitted(self):
        self.add_external()
        with self.assertRaisesRegex(ValueError, "external data is missing"):
            self.create()
        manifest = self.create(require_external=False)
        self.assertEqual(manifest["externalCoverage"], "incomplete")
        with self.assertRaisesRegex(ValueError, "incomplete"):
            verify(self.archive)
        verify(self.archive, require_external=False)

    def test_private_trust_buckets_and_orphans_are_discovered_without_keys(self):
        self.add_trust_material_ledger()
        resources = external_inventory(self.data / "app-cloud.db")
        self.assertEqual({r["name"]: r["objectCount"] for r in resources}, {"private-current": 2, "private-previous": 1})
        self.assertTrue(all(r["scope"] == "trust-private-materials" and r["kind"] == "s3" and r["id"].startswith("trust-materials:") for r in resources))
        self.assertNotIn("materials/", json.dumps(resources))
        self.assertNotIn("material-active", json.dumps(resources))
        with self.assertRaisesRegex(ValueError, "external data is missing"):
            self.create()
        self.create(require_external=False)
        with self.assertRaisesRegex(ValueError, "incomplete"):
            verify(self.archive)
        self.assertEqual(verify(self.archive, require_external=False)["externalCoverage"], "incomplete")

    def test_verified_private_trust_artifacts_restore_with_the_ledger(self):
        self.add_trust_material_ledger()
        resources = external_inventory(self.data / "app-cloud.db")
        for index, resource in enumerate(resources):
            artifacts = []
            for purpose in ("data", "configuration"):
                path = self.root / f"trust-{index}-{purpose}.backup"
                path.write_bytes(f"opaque encrypted bucket fixture {index} {purpose}".encode())
                artifacts.append({"path": path.name, "sha256": digest(path), "purpose": purpose})
            resource.update(capturedAt="2026-09-27T12:00:00Z", restoreProcedure="restore private ciphertext and private bucket policy; then verify Trust access", artifacts=artifacts)
        spec = self.root / "trust-external.json"
        spec.write_text(json.dumps({"schemaVersion": 1, "resources": resources}))
        self.create(external_spec=spec)
        verify(self.archive)
        restore(self.archive, self.root / "trust-restored", True)
        recovered = external_inventory(self.root / "trust-restored/data/app-cloud.db")
        self.assertEqual(sum(r["objectCount"] for r in recovered), 3)
        self.assertEqual(len(list((self.root / "trust-restored/external").glob("*/*"))), 4)

    def test_signed_inventory_cannot_omit_private_trust_buckets(self):
        self.add_trust_material_ledger()
        self.create(require_external=False)
        def omit_trust(files):
            manifest = json.loads(files["manifest.json"])
            manifest.update(externalResources=[], externalCoverage="artifacts-included")
            manifest["manifestHMAC"] = manifest_signature(manifest)
            files["manifest.json"] = json.dumps(manifest).encode()
        with self.assertRaisesRegex(ValueError, "actual control metadata"):
            verify(self.rewrite(omit_trust))

    def test_legacy_inline_trust_materials_need_no_external_artifact(self):
        with sqlite3.connect(self.data / "app-cloud.db") as db:
            db.executescript("CREATE TABLE trust_materials(id TEXT PRIMARY KEY,body BLOB); INSERT INTO trust_materials VALUES('inline',X'010203');")
        db.close()
        self.assertEqual(self.create()["externalResources"], [])
        verify(self.archive)

    def test_external_artifacts_validated_and_restored_separately(self):
        self.add_external()
        specs = []
        for kind, ident in (("postgres", "db-1"), ("s3", "bucket-1")):
            artifacts = []
            for purpose in ("data", "configuration"):
                path = self.root / f"{kind}-{purpose}.backup"
                path.write_bytes(f"opaque fixture {kind} {purpose}".encode())
                artifacts.append({"path": path.name, "sha256": digest(path), "purpose": purpose})
            specs.append({"kind": kind, "id": ident, "capturedAt": "2026-09-27T12:00:00Z", "restoreProcedure": "isolated restore plus actual SQL/object read acceptance", "artifacts": artifacts})
        spec_path = self.root / "external.json"
        spec_path.write_text(json.dumps({"schemaVersion": 1, "resources": specs}))
        self.create(external_spec=spec_path)
        verify(self.archive)
        restore(self.archive, self.root / "restored", True)
        self.assertEqual(len(list((self.root / "restored/external").glob("*/*"))), 4)
        # Checksumming a dump is not a claim that its database restored successfully.
        self.assertNotIn("restoreVerified", json.dumps(verify(self.archive)))
        self.archive.unlink()
        (self.root / "postgres-data.backup").write_bytes(b"changed")
        with self.assertRaisesRegex(ValueError, "checksum mismatch"):
            self.create(external_spec=spec_path)

    def test_existing_backup_never_overwritten_and_size_limit(self):
        self.create()
        original = self.archive.read_bytes()
        with self.assertRaises(ValueError):
            self.create()
        self.assertEqual(self.archive.read_bytes(), original)
        with self.assertRaisesRegex(ValueError, "limit"):
            verify(self.archive, max_bytes=100)

    def test_foreign_key_violation_prevents_backup(self):
        with sqlite3.connect(self.data / "app-cloud.db") as db:
            db.executescript("CREATE TABLE parent(id INTEGER PRIMARY KEY); CREATE TABLE child(parent_id REFERENCES parent(id)); INSERT INTO child VALUES(99);")
        with self.assertRaisesRegex(ValueError, "foreign key"):
            self.create()

    def test_manifest_authentication_rejects_resealed_content_hash(self):
        self.create()
        def tamper(files):
            files["data/apps/proof.bin"] = b"tampered"
            manifest = json.loads(files["manifest.json"])
            for entry in manifest["files"]:
                if entry["path"] == "data/apps/proof.bin":
                    entry.update(size=8, sha256=hashlib.sha256(b"tampered").hexdigest())
            files["manifest.json"] = json.dumps(manifest).encode()
        with self.assertRaisesRegex(ValueError, "authentication"):
            verify(self.rewrite(tamper))

    def test_directory_mutation_during_backup_is_refused(self):
        def change(*args):
            (self.data / "new-record.bin").write_bytes(b"concurrent writer")
            return []
        with patch("platform_backup.copy_external", side_effect=change):
            with self.assertRaisesRegex(ValueError, "changed during capture"):
                self.create()
        self.assertFalse(self.archive.exists())

    def test_recovery_drill_reads_tables_without_starting_service(self):
        self.create()
        report = drill(self.archive, self.root / "drill")
        self.assertEqual(len(report["databases"]), 3)
        self.assertFalse(report["serviceStarted"])
        self.assertFalse(report["productionRestoreAccepted"])
        self.assertEqual(report["databases"][0]["tables"], [{"table": "records", "rows": 1}])

    def test_cli_backup_verify_and_recovery_report(self):
        script = Path(__file__).with_name("platform_backup.py")
        def command(*arguments):
            result = subprocess.run([sys.executable, str(script), *map(str, arguments)], capture_output=True, text=True, check=True)
            return json.loads(result.stdout)
        report = command("backup", "--data-dir", self.data, "--output", self.archive, "--service-stopped", "--release", "cli-fixture")
        self.assertEqual(report["verifiedFiles"], 4)
        self.assertFalse(report["productionRestoreAccepted"])
        self.assertEqual(command("verify", self.archive)["release"], "cli-fixture")
        self.assertEqual(command("inventory", self.data / "app-cloud.db")["resources"], [])
        output = self.root / "drill.json"
        result = subprocess.run([sys.executable, str(script.with_name("recovery_drill.py")), str(self.archive), str(self.root / "cli-drill"), "--output", str(output)], capture_output=True, text=True, check=True)
        self.assertEqual(json.loads(result.stdout)["inspectedDatabases"], 3)
        self.assertFalse(json.loads(output.read_text())["productionRestoreAccepted"])


if __name__ == "__main__":
    unittest.main()
