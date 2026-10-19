"""Offline regression: the atomic local JSON writer must not damage other files.

Synthetic temporary files only; no wallet, browser, network, or production state.
"""
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import swarmmemo as memo


class AtomicStateWriteTests(unittest.TestCase):
    def test_existing_unrelated_same_stem_tmp_survives(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "cursor.json"
            unrelated = Path(directory) / "cursor.tmp"
            original = b"ORIGINAL UNRELATED LOCAL DOCUMENT\n"
            unrelated.write_bytes(original)
            memo.write_private(target, {"cursor": "next"})
            self.assertEqual(unrelated.read_bytes(), original)
            self.assertEqual(json.loads(target.read_text()), {"cursor": "next"})
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)

    def test_failed_replace_preserves_previous_state_and_cleans_stage(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "cursor.tmp"  # valid user-specified cursor filename
            original = '{"cursor":"old"}\n'
            target.write_text(original, encoding="utf-8")
            with patch.object(memo.os, "replace", side_effect=OSError("synthetic failed rename")):
                with self.assertRaisesRegex(OSError, "synthetic failed rename"):
                    memo.write_private(target, {"cursor": "new"})
            self.assertEqual(target.read_text(encoding="utf-8"), original)
            self.assertEqual(list(Path(directory).iterdir()), [target])

    def test_two_writes_replace_only_target_without_leftover_temp_files(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "inbox.json"
            memo.write_private(target, {"a": 1})
            memo.write_private(target, {"a": 2})
            self.assertEqual(json.loads(target.read_text()), {"a": 2})
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)
            self.assertEqual(list(Path(directory).iterdir()), [target])


if __name__ == "__main__":
    unittest.main()
