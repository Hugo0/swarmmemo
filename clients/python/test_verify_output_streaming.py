"""Offline CLI regression: large verification output is hashed in bounded chunks.

A verifying agent may capture multi-gigabyte Foundry/Cargo/test logs. --output
must never allocate the entire file or let locally available RAM determine whether
the signed verification can be published. No network requests or user secrets.
"""
import contextlib
import hashlib
import io
import json
import unittest
from unittest import mock

import swarmmemo as memo


class GuardedOutput(io.BytesIO):
    def __init__(self, value):
        super().__init__(value)
        self.read_sizes = []

    def read(self, size=-1):
        self.read_sizes.append(size)
        if size is None or size < 0 or size > 1024 * 1024:
            raise AssertionError("verify --output attempted an unbounded read")
        return super().read(size)


class VerifyOutputStreamingTests(unittest.TestCase):
    def test_verify_publish_large_output_hashes_in_bounded_chunks(self):
        # More than 2 MiB of synthetic output crosses multiple 1 MiB reads.
        expected_bytes = b"SYNTHETIC_VERIFY_OUTPUT\x00" * 110000
        expected = hashlib.sha256(expected_bytes).hexdigest()
        spy = GuardedOutput(expected_bytes)
        submitted = []

        def local_only_publish(client, command, environment, digest, checks, limits,
                               repo=None, commit=None, file_sha256=None, file=None,
                               request_id=None, supersedes=None, **fields):
            submitted.append({"sha256": digest, "command": command,
                              "repo": repo, "checks": checks})
            return {"ok": True, "offline_fake": True}

        def fake_open(name, mode="r", *args, **kwargs):
            self.assertEqual(name, "/virtual/no-live-file/large-synthetic-test.log")
            self.assertEqual(mode, "rb")
            return spy

        with mock.patch.object(memo, "open", fake_open, create=True), \
             mock.patch.object(memo.Client, "verify_publish", local_only_publish), \
             contextlib.redirect_stdout(io.StringIO()) as stdout:
            status = memo.main([
                "verify", "publish",
                "--repo", "https://github.com/Hugo0/swarmmemo",
                "--commit", "0" * 40,
                "--command", "isolated unittest -v",
                "--env", "local synthetic client only, no network",
                "--output", "/virtual/no-live-file/large-synthetic-test.log",
                "--check", "tests=pass:original offline regression",
                "--limits", "no upstream production calls"
            ])

        self.assertEqual(status, 0)
        self.assertEqual(len(submitted), 1)
        self.assertEqual(submitted[0]["sha256"], expected)
        self.assertEqual(submitted[0]["command"], "isolated unittest -v")
        self.assertTrue(submitted[0]["checks"])
        self.assertTrue(json.loads(stdout.getvalue())["offline_fake"])
        self.assertGreaterEqual(len(spy.read_sizes), 3)
        self.assertTrue(all(0 < n <= 1024 * 1024 for n in spy.read_sizes))


if __name__ == "__main__":
    unittest.main()
