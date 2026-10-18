"""Python wallet-key post-stat growth parity regression; strictly synthetic.

No user wallet keys, no wallet signing, no internet, no credentials and no
private customer bytes. The synthetic 1 scalar is only a local test fixture.
"""
from __future__ import annotations

import os
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest import TestCase, main
from unittest.mock import patch

import swarmmemo as memo

SYNTHETIC_SCALAR_TEXT = "0" * 63 + "1"


class FileSizeAfterStat(TestCase):
    def test_rejects_file_grown_above_256_after_stat(self):
        with TemporaryDirectory(prefix="swarmmemo-py-wallet-bound-") as directory:
            path = Path(directory) / "synthetic-wallet.txt"
            path.write_text(SYNTHETIC_SCALAR_TEXT, encoding="ascii")
            os.chmod(path, 0o600)
            real_fstat = os.fstat
            injected_growth = []

            def stat_then_grow(fd):
                original_stat = real_fstat(fd)
                if not injected_growth:
                    with path.open("ab") as stream:
                        stream.write(b" " * 193)  # 64 + 193 == 257
                    injected_growth.append(True)
                return original_stat

            # Returns original size 64, but original file now really has 257.
            # Strip() would otherwise hide a size-limit bypass.
            with patch.object(memo.os, "fstat", side_effect=stat_then_grow):
                with self.assertRaisesRegex(ValueError, "small regular file"):
                    memo.load_wallet_key(path)
            self.assertTrue(injected_growth)
            self.assertEqual(path.stat().st_size, 257)

    def test_unchanged_normal_owner_only_synthetic_file_still_accepted(self):
        with TemporaryDirectory(prefix="swarmmemo-py-wallet-valid-") as directory:
            path = Path(directory) / "synthetic-wallet.txt"
            path.write_text(SYNTHETIC_SCALAR_TEXT, encoding="ascii")
            os.chmod(path, 0o600)
            actual = memo.load_wallet_key(path)
            self.assertEqual(actual, bytes.fromhex(SYNTHETIC_SCALAR_TEXT))


if __name__ == "__main__":
    main(verbosity=2)
