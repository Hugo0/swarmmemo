"""Synthetic-only VM1 regression for a legal short os.read from a growing
owner-only file in the sponsor-invited SwarmMemo PR #16 shared helper.

No user secrets, wallet signature, mainnet request, real credentials or API.
"""
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import swarmmemo as sm


class SharedReaderShortReads(unittest.TestCase):
    def check_growing_file(self, cap, initial, extra, invoke):
        with tempfile.TemporaryDirectory(prefix="vm1-swm-short-read-") as directory:
            path=Path(directory)/"test-only.txt"
            path.write_bytes(initial)
            path.chmod(0o600)
            real_fstat=os.fstat
            real_read=os.read
            read_calls=[]
            growth_applied=[]
            def grow_after_initial_stat(fd):
                result=real_fstat(fd)
                if not growth_applied:
                    with path.open("ab") as output:
                        output.write(extra)
                    growth_applied.append(True)
                return result
            def legal_short_read(fd,n):
                result=real_read(fd,min(64,n))
                read_calls.append((n,len(result)))
                return result
            with patch.object(sm.os,"fstat",side_effect=grow_after_initial_stat), patch.object(sm.os,"read",side_effect=legal_short_read):
                with self.assertRaises(ValueError):
                    invoke(path,cap)
            self.assertTrue(growth_applied)
            self.assertGreater(len(initial)+len(extra),cap)
            self.assertEqual(path.stat().st_size,len(initial)+len(extra))
            self.assertEqual(path.stat().st_mode & 0o077,0)

    def test_01_wallet_loader_rejects_257byte_file_even_if_first_read_returns_only64(self):
        self.check_growing_file(
            256, b"11"*32, b" "*193,
            lambda path,cap: sm.load_wallet_key(path))

    def test_02_identity_common_helper_rejects_cap128_overgrowth_after_short_read(self):
        self.check_growing_file(
            128, b'{"synthetic":"only"}', b" "*140,
            lambda path,cap: sm.read_owner_key_bytes(path,cap,
                invalid_message="oversize synthetic identity file",
                open_message="synthetic identity missing"))

    def test_03_normal_64byte_synthetic_wallet_still_parses(self):
        with tempfile.TemporaryDirectory(prefix="vm1-swm-valid-only-") as directory:
            path=Path(directory)/"valid.txt"
            path.write_bytes(b"11"*32)
            path.chmod(0o600)
            self.assertEqual(sm.load_wallet_key(path),bytes.fromhex("11"*32))


if __name__=="__main__":
    unittest.main(verbosity=2)
