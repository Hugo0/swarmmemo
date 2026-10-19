"""Synthetic regression: verified log checkpoint state survives failed writes.

An old direct open('w') can destroy an existing verifier checkpoint when
a write fails. All input is synthetic; no production keys or network.
"""
import base64
import json
import os
import tempfile
from pathlib import Path
import unittest
from unittest import mock

import verify_log as v


class CheckpointStateAtomicityTests(unittest.TestCase):
    def setUp(self):
        self.work = tempfile.TemporaryDirectory(prefix="swarmmemo-checkpoint-state-")
        self.addCleanup(self.work.cleanup)
        self.root = bytes(range(32))
        self.state = Path(self.work.name) / "checkpoint.json"
        self.old = {
            "origin": "swarmmemo.com/log",
            "size": 7,
            "root": base64.b64encode(self.root).decode(),
            "key": "SYNTHETIC_NOT_A_PRIVATE_KEY",
        }
        self.state.write_text(json.dumps(self.old, sort_keys=True), encoding="utf-8")
        self.before = self.state.read_bytes()
        self.verifier = v.Verifier("https://swarmmemo.com", self.old["key"], str(self.state))

    def test_write_failure_preserves_previously_verified_checkpoint_bytes(self):
        with mock.patch.object(v.json, "dump", side_effect=OSError("synthetic ENOSPC")):
            with self.assertRaisesRegex(OSError,"ENOSPC"):
                self.verifier.remember(self.old["origin"], 7, self.root)
        self.assertEqual(self.state.read_bytes(), self.before,"Never destroy the previously checked log pin")
        self.assertEqual(json.loads(self.state.read_text()), self.old)
        self.assertEqual(set(Path(self.work.name).iterdir()), {self.state},"No stranded temporary file")

    def test_identical_verified_checkpoint_is_written_with_owner_only_permissions(self):
        os.chmod(self.state, 0o600)
        self.verifier.remember(self.old["origin"], 7, self.root)
        self.assertEqual(json.loads(self.state.read_text()), self.old)
        if os.name == "posix":
            self.assertEqual(self.state.stat().st_mode & 0o777, 0o600)
        self.assertEqual(set(Path(self.work.name).iterdir()), {self.state})

    @unittest.skipUnless(hasattr(os, "symlink"), "symlink not available")
    def test_existing_state_symlink_is_replaced_without_clobbering_target(self):
        target = Path(self.work.name) / "elsewhere.json"
        target.write_bytes(self.before)
        self.state.unlink()
        os.symlink(target, self.state)
        self.verifier.remember(self.old["origin"], 7, self.root)
        self.assertFalse(self.state.is_symlink())
        self.assertEqual(target.read_bytes(), self.before)
        self.assertEqual(json.loads(self.state.read_text()), self.old)
        self.assertEqual(set(Path(self.work.name).iterdir()), {self.state, target})


if __name__ == "__main__":
    unittest.main()
