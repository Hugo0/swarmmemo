"""PR #45 follow-up (realbtr's checklist): a key-less room read answered not_found adds a
structured "hint" to the error JSON on stderr, the same words for a missing room and a
private one. No key is made, no second request is sent, stdout stays empty, the exit is 1,
and every other case (a signed read, another operation, another error) is unchanged."""
import contextlib
import io
import json
import os
import tempfile
import unittest
from unittest.mock import patch

import swarmmemo as memo


class PrivateReadHint(unittest.TestCase):
    def run_cli(self, argv, code="not_found", key=False):
        calls = []

        def fake(self, path, body=None):
            calls.append(path)
            raise memo.APIError(404, code, "nothing here")
        out, err = io.StringIO(), io.StringIO()
        with tempfile.TemporaryDirectory() as home, patch.dict(os.environ, {"SWARMMEMO_HOME": home, "SWARMMEMO_KEY": ""}), \
                patch.object(memo.Client, "_request", fake), contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            if key:
                with patch.object(memo, "load_key", return_value=memo.crypto()[0].from_private_bytes(bytes(32))):
                    rc = memo.main(["--key", "k.json", *argv])
            else:
                rc = memo.main(argv)
            made = os.listdir(home)
        self.assertEqual(rc, 1)
        self.assertEqual(out.getvalue(), "")
        self.assertEqual(len(calls), 1)
        return json.loads(err.getvalue().strip().splitlines()[0]), made

    def test_keyless_read_not_found_hints(self):
        error, made = self.run_cli(["read", "secret-room"])
        self.assertEqual(error["error"], "not_found")
        self.assertEqual(error["hint"], memo.PRIVATE_READ_HINT)
        self.assertTrue(error["hint"].endswith("/protocol.md#read-a-private-room"))
        self.assertEqual(set(error) - {"hint"}, {"error", "message", "retry_after", "next_open_at"})
        self.assertNotIn("key.json", made)

    def test_signed_read_not_found_no_hint(self):
        error, _ = self.run_cli(["read", "secret-room"], key=True)
        self.assertNotIn("hint", error)

    def test_other_operation_not_found_no_hint(self):
        error, _ = self.run_cli(["agent", "nobody"])
        self.assertNotIn("hint", error)

    def test_read_other_error_no_hint(self):
        error, _ = self.run_cli(["read", "lobby"], code="request_rate")
        self.assertNotIn("hint", error)


if __name__ == "__main__":
    unittest.main()
