"""Offline security regression: same shared bounded reader protects identity and wallet.

These use only disposable synthetic generated test keys in an owner-only temp
directory. No personal wallet, user account, mainnet, or network read.
"""
from pathlib import Path
import os
import tempfile
import unittest
from unittest.mock import patch
import swarmmemo as memo


class OriginalOwnerOnlyReadParity(unittest.TestCase):
    def test_identity_key_growth_after_fstat_rejected(self):
        with tempfile.TemporaryDirectory(prefix="vm2-test-identity-") as temp:
            path=Path(temp)/"identity.json"
            memo.keygen(path)  # disposable synthetic Ed25519 key, never persisted outside tempfile
            size=path.stat().st_size
            before=memo.os.fstat
            did_grow=False
            def race(fd):
                nonlocal did_grow
                st=before(fd)
                if not did_grow:
                    did_grow=True
                    with path.open("ab") as f:f.write(b" "*(16385-size))
                return st
            with patch.object(memo.os,"fstat",side_effect=race):
                with self.assertRaises(memo.InputTooLarge):
                    memo.load_key(path)
            self.assertTrue(did_grow)

    def test_wallet_key_growth_after_fstat_rejected(self):
        with tempfile.TemporaryDirectory(prefix="vm2-test-wallet-") as temp:
            path=Path(temp)/"synthetic-wallet.txt"
            original=b"42"*32
            path.write_bytes(original)
            path.chmod(0o600)
            before=memo.os.fstat
            did_grow=False
            def race(fd):
                nonlocal did_grow
                st=before(fd)
                if not did_grow:
                    did_grow=True
                    with path.open("ab") as f:f.write(b" "*(257-len(original)))
                return st
            with patch.object(memo.os,"fstat",side_effect=race):
                with self.assertRaises(ValueError) as caught:
                    memo.load_wallet_key(path)
            self.assertTrue(did_grow)
            self.assertNotIn(original.decode(),str(caught.exception))

    def test_normal_synthetic_identity_and_wallet_still_load(self):
        with tempfile.TemporaryDirectory(prefix="vm2-test-owner-") as temp:
            identity=Path(temp)/"agent.json"
            wallet=Path(temp)/"wallet.txt"
            memo.keygen(identity)
            wallet.write_bytes(b"42"*32)
            wallet.chmod(0o600)
            self.assertIsNotNone(memo.load_key(identity))
            self.assertEqual(memo.load_wallet_key(wallet),bytes.fromhex("42"*32))


if __name__=="__main__":
    unittest.main()
