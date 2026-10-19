"""Offline regression: acknowledged private chat/seal state must survive rename.

The source already syncs the unique temporary file. On POSIX, sync the parent
directory too, so a successful return means the replacement directory entry
was also requested durable. No network, credentials or customer data.
"""
from __future__ import annotations

import json
import os
import stat
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import swarmmemo as memo


@unittest.skipUnless(os.name == "posix" and hasattr(os, "O_DIRECTORY"), "POSIX directory durability")
class PrivateStateDirectoryDurability(unittest.TestCase):
    def test_new_private_state_syncs_file_then_containing_directory(self):
        with tempfile.TemporaryDirectory(prefix="vm1-offline-fsync-") as folder:
            path = Path(folder) / "chat.json"
            real_fsync = os.fsync
            kinds = []

            def record_fsync(fd):
                mode = os.fstat(fd).st_mode
                kinds.append("dir" if stat.S_ISDIR(mode) else "file")
                return real_fsync(fd)

            with patch.object(memo.os, "fsync", side_effect=record_fsync):
                memo.write_private(path, {"room": "~a", "cursor": "initial"})

            self.assertEqual(kinds, ["file", "dir"])
            self.assertEqual(json.loads(path.read_text()), {"room": "~a", "cursor": "initial"})
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertEqual([p.name for p in Path(folder).iterdir()], ["chat.json"])

    def test_second_private_state_replacement_syncs_parent_directory(self):
        with tempfile.TemporaryDirectory(prefix="vm1-offline-fsync-") as folder:
            path = Path(folder) / "seal-state.json"
            memo.write_private(path, {"keys": ["old"]})
            real_fsync = os.fsync
            kinds = []

            def observe(fd):
                kinds.append("dir" if stat.S_ISDIR(os.fstat(fd).st_mode) else "file")
                return real_fsync(fd)

            with patch.object(memo.os, "fsync", side_effect=observe):
                memo.write_private(path, {"keys": ["new"]})
            self.assertEqual(kinds, ["file", "dir"])
            self.assertEqual(json.loads(path.read_text()), {"keys": ["new"]})
            self.assertEqual([p.name for p in Path(folder).iterdir()], ["seal-state.json"])

    def test_directory_fsync_failure_does_not_silently_claim_durability(self):
        with tempfile.TemporaryDirectory(prefix="vm1-offline-fsync-") as folder:
            path = Path(folder) / "cursor.json"
            memo.write_private(path, {"cursor": "old"})
            real_fsync = os.fsync
            kinds = []

            def fail_directory_only(fd):
                if stat.S_ISDIR(os.fstat(fd).st_mode):
                    kinds.append("dir")
                    raise OSError("synthetic directory fsync failure")
                kinds.append("file")
                return real_fsync(fd)

            with patch.object(memo.os, "fsync", side_effect=fail_directory_only):
                with self.assertRaises(OSError):
                    memo.write_private(path, {"cursor": "new"})
            self.assertEqual(kinds, ["file", "dir"])
            # Replacement happened; only a successful fsync can promise its
            # name persists after sudden power loss. No rollback guess.
            self.assertEqual(json.loads(path.read_text()), {"cursor": "new"})
            self.assertEqual([p.name for p in Path(folder).iterdir()], ["cursor.json"])


if __name__ == "__main__":
    unittest.main()
