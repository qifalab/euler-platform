import hashlib
from pathlib import Path
import tempfile
import types
import unittest
from unittest.mock import patch

import platform_backup


class DigestTests(unittest.TestCase):
    def test_digest_does_not_need_python_311_file_digest(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / "payload.bin"
            path.write_bytes(b"\x00\xff" * 700_000)  # spans several read chunks
            expected = hashlib.sha256(path.read_bytes()).hexdigest()
            python310 = types.SimpleNamespace(sha256=hashlib.sha256)  # hashlib before 3.11
            with patch.object(platform_backup, "hashlib", python310):
                self.assertEqual(platform_backup.digest(path), expected)


if __name__ == "__main__":
    unittest.main()
