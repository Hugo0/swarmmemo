"""keygen makes the folder its key goes in, owner-only, as the other key and state
writers do; an existing folder keeps its mode and an existing key is still refused
(patch result a7c613af by sly-ledger-31)."""
import os
from pathlib import Path
import stat
import tempfile
import unittest

import swarmmemo as memo


class KeygenFolderTests(unittest.TestCase):
    def setUp(self):
        folder = tempfile.TemporaryDirectory(); self.addCleanup(folder.cleanup)
        self.dir = Path(folder.name)

    def test_makes_a_missing_folder_owner_only(self):
        key = self.dir / "fresh" / "nested" / "key.json"
        record = memo.keygen(key)
        self.assertTrue(key.is_file())
        self.assertEqual(stat.S_IMODE(key.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(key.parent.stat().st_mode) & 0o077, 0)
        self.assertEqual(len(record["fingerprint"]), 64)

    def test_takes_a_string_path(self):
        key = self.dir / "as-str" / "key.json"
        memo.keygen(str(key))
        self.assertEqual(stat.S_IMODE(key.stat().st_mode), 0o600)

    def test_existing_folder_and_key_unchanged(self):
        folder = self.dir / "shared"; folder.mkdir(mode=0o755); os.chmod(folder, 0o755)
        key = folder / "key.json"
        memo.keygen(key)
        self.assertEqual(stat.S_IMODE(folder.stat().st_mode), 0o755)
        with self.assertRaises(FileExistsError):
            memo.keygen(key)


if __name__ == "__main__":
    unittest.main()
