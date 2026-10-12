import os
from pathlib import Path
import sqlite3
import tempfile
import unittest

from sqlite_snapshot import restore, snapshot, verify


class SnapshotTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.database = self.root / "live.db"
        self.backup = self.root / "snapshot.db"
        self.db = sqlite3.connect(self.database)
        self.db.execute("PRAGMA journal_mode=WAL")
        self.db.execute("CREATE TABLE records (id INTEGER PRIMARY KEY, value TEXT NOT NULL)")
        self.db.execute("INSERT INTO records VALUES (1, 'before')")
        self.db.commit()
        self.addCleanup(self.db.close)

    def read(self, path):
        db = sqlite3.connect(path)
        try:
            return db.execute("SELECT value FROM records ORDER BY id").fetchall()
        finally:
            db.close()

    def test_backup_includes_committed_wal_and_is_independent(self):
        self.assertTrue(Path(str(self.database) + "-wal").exists())
        snapshot(self.database, self.backup)
        self.db.execute("INSERT INTO records VALUES (2, 'after')")
        self.db.commit()
        verify(self.backup)
        self.assertEqual(self.read(self.backup), [("before",)])
        self.assertFalse(Path(str(self.backup) + "-wal").exists())
        self.assertEqual(os.stat(self.backup).st_mode & 0o777, 0o600)

    def test_existing_backup_and_missing_source_are_refused(self):
        snapshot(self.database, self.backup)
        original = self.backup.read_bytes()
        with self.assertRaises(ValueError):
            snapshot(self.database, self.backup)
        self.assertEqual(self.backup.read_bytes(), original)
        with self.assertRaises(ValueError):
            snapshot(self.root / "missing.db", self.root / "new.db")
        self.assertFalse((self.root / "missing.db").exists())

    def test_restore_preserves_previous_database(self):
        snapshot(self.database, self.backup)
        self.db.execute("INSERT INTO records VALUES (2, 'after')")
        self.db.commit()
        self.db.close()
        rollback = restore(self.backup, self.database, service_stopped=True)
        self.assertEqual(self.read(self.database), [("before",)])
        self.assertEqual(self.read(rollback), [("before",), ("after",)])

    def test_invalid_snapshot_and_missing_stop_ack_cannot_replace_database(self):
        self.db.close()
        self.backup.write_bytes(b"not a sqlite database")
        original = self.database.read_bytes()
        with self.assertRaises(sqlite3.DatabaseError):
            restore(self.backup, self.database, service_stopped=True)
        self.assertEqual(self.database.read_bytes(), original)
        with self.assertRaises(ValueError):
            restore(self.backup, self.database, service_stopped=False)
        self.assertEqual(self.database.read_bytes(), original)

    def test_active_wal_prevents_restore(self):
        snapshot(self.database, self.backup)
        with self.assertRaises(ValueError):
            restore(self.backup, self.database, service_stopped=True)
        self.assertEqual(self.read(self.database), [("before",)])


if __name__ == "__main__":
    unittest.main()
