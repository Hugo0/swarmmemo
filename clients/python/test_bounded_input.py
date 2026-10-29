"""Local input is read with a cap (C416): stdin `-`, message and welcome files, and local
JSON state (key file, chat settings, conversation state) are read up to cap+1 and refused
with a clear message, never loaded whole. The message names neither the input nor its path."""
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import swarmmemo  # noqa: E402
import verify_log  # noqa: E402

# Stubs the network: memory put echoes the value it would have stored.
DRIVER = """
import sys
sys.path.insert(0, %r)
import swarmmemo
def service_call(self, service, method, data, cost, request_id=None):
    sys.stderr.write("SENT %%d\\n" %% len(data["value"]))
    return {"ok": True}
swarmmemo.Client.service_call = service_call
sys.exit(swarmmemo.main(sys.argv[1:]))
""" % str(HERE)


def cli(args, stdin_bytes):
    return subprocess.run([sys.executable, "-c", DRIVER, *args], input=stdin_bytes, capture_output=True, timeout=60)


class ReadBounded(unittest.TestCase):
    def test_stdin_at_cap_is_read(self):
        with mock.patch.object(sys, "stdin", io.StringIO("x" * 64)):
            self.assertEqual(swarmmemo.read_bounded("-", "the value", 64), "x" * 64)

    def test_stdin_over_cap_is_refused_without_its_content(self):
        with mock.patch.object(sys, "stdin", io.StringIO("secret" * 20)):
            with self.assertRaises(swarmmemo.InputTooLarge) as caught:
                swarmmemo.read_bounded("-", "the value", 64)
        self.assertIn("the value is over 64 bytes", str(caught.exception))
        self.assertNotIn("secret", str(caught.exception))

    def test_cap_counts_utf8_bytes(self):
        with mock.patch.object(sys, "stdin", io.StringIO("é" * 40)):  # 40 characters, 80 bytes
            with self.assertRaises(swarmmemo.InputTooLarge):
                swarmmemo.read_bounded("-", "the value", 64)

    def test_file_over_cap_is_refused_without_its_path(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "private-name.txt"
            path.write_text("x" * (swarmmemo.INPUT_BYTES + 1), encoding="utf-8")
            with self.assertRaises(swarmmemo.InputTooLarge) as caught:
                swarmmemo.chat_text(str(path))
            self.assertNotIn("private-name", str(caught.exception))
            self.assertIn("the message is over 2 MiB", str(caught.exception))
            path.write_text("hello\r\nthere", encoding="utf-8")
            self.assertEqual(swarmmemo.chat_text(str(path)), "hello\nthere")  # as read_text did

    def test_local_state_over_cap_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(swarmmemo, "STATE_BYTES", 32):
            path = Path(tmp) / "state.json"
            path.write_text(json.dumps({"cursor": "c"}), encoding="utf-8")
            self.assertEqual(swarmmemo.read_private(path, {}), {"cursor": "c"})
            path.write_text(json.dumps({"cursor": "c" * 64}), encoding="utf-8")
            with self.assertRaises(swarmmemo.InputTooLarge):
                swarmmemo.read_private(path, {})

    def test_key_file_over_cap_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "key.json"
            swarmmemo.keygen(path)
            self.assertTrue(swarmmemo.load_key(path))  # a real key file is far below the cap
            with mock.patch.object(swarmmemo, "STATE_BYTES", 32), self.assertRaises(swarmmemo.InputTooLarge) as caught:
                swarmmemo.load_key(path)
            self.assertNotIn("private_key", str(caught.exception))

    def test_promise_file_over_cap_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(verify_log, "PROMISE_FILE_BYTES", 16):
            path = Path(tmp) / "promise.txt"
            path.write_text("note", encoding="utf-8")
            self.assertEqual(verify_log.read_promise(str(path)), "note")
            path.write_text("n" * 17, encoding="utf-8")
            with self.assertRaises(verify_log.VerifyError):
                verify_log.read_promise(str(path))


class CLI(unittest.TestCase):
    def test_normal_stdin_is_sent(self):
        done = cli(["memory", "put", "note", "-"], b"hello")
        self.assertEqual(done.returncode, 0, done.stderr.decode("utf-8", "replace"))
        self.assertIn("SENT 5", done.stderr.decode("utf-8"))

    def test_oversize_stdin_is_refused_with_a_clear_message(self):
        done = cli(["memory", "put", "note", "-"], b"x" * (swarmmemo.INPUT_BYTES + 1))
        err = done.stderr.decode("utf-8", "replace")
        self.assertEqual(done.returncode, 1)
        self.assertNotIn("SENT", err)
        self.assertIn("SwarmMemo: the value is over 2 MiB", err)
        self.assertNotIn("request failed", err)

    def test_client_refusal_names_its_reason(self):
        # C624: the client's own refusal of an input prints its message, not a bare "ValueError".
        driver = DRIVER.replace('sys.stderr.write("SENT %d\\n" % len(data["value"]))',
                                'raise swarmmemo.UsageError("note is 1 to 80 characters")')
        done = subprocess.run([sys.executable, "-c", driver, "memory", "put", "note", "-"], input=b"hi",
                              capture_output=True, timeout=60)
        err = done.stderr.decode("utf-8", "replace")
        self.assertEqual(done.returncode, 1, err)
        self.assertIn("SwarmMemo: note is 1 to 80 characters", err)
        self.assertNotIn("request failed", err)

    def test_oversize_raw_command_is_refused(self):
        done = cli(["command", "-"], b"{" + b" " * swarmmemo.INPUT_BYTES + b"}")
        err = done.stderr.decode("utf-8", "replace")
        self.assertEqual(done.returncode, 1, err)
        self.assertIn("SwarmMemo: the command is over 2 MiB", err)


if __name__ == "__main__":
    unittest.main()
