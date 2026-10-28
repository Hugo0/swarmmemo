"""C546 signed by default: a keyless CLI write makes one device key, owner-only, and
signs with it; reads, --anonymous and the GET transports never use or make one, and a
retry of the saved request is byte-identical. Every test has its own SWARMMEMO_HOME."""
import contextlib
import io
import json
import os
from pathlib import Path
import shlex
import tempfile
import unittest
from unittest.mock import patch

import swarmmemo as memo

README = Path(__file__).resolve().parent / "README.md"


class DeviceKeyTests(unittest.TestCase):
    def setUp(self):
        folder = tempfile.TemporaryDirectory(); self.addCleanup(folder.cleanup)
        self.dir = Path(folder.name)
        self.home = self.dir / "home"
        env = patch.dict(os.environ, {"SWARMMEMO_HOME": str(self.home)}); env.start(); self.addCleanup(env.stop)
        os.environ.pop("SWARMMEMO_KEY", None)
        self.sent = []
        board = patch.object(memo.Client, "_request", lambda client, path, body=None: self.board(path, body))
        board.start(); self.addCleanup(board.stop)

    def board(self, path, body):
        self.sent.append((path, None if body is None else json.loads(json.dumps(body))))
        return {"ok": True, "receipt": {"id": "r%d" % len(self.sent)}, "messages": []}

    @property
    def key_file(self):
        return self.home / "key.json"

    def cli(self, *argv, code=0):
        with contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(memo.main(list(argv)), code, err.getvalue())
        return out.getvalue(), err.getvalue()

    def test_first_write_makes_an_owner_only_key_and_signs_and_the_next_reuses_it(self):
        _, err = self.cli("post", "lobby", "main", "hello")
        self.assertEqual(self.key_file.stat().st_mode & 0o777, 0o600)
        self.assertEqual(self.home.stat().st_mode & 0o777, 0o700)
        key = memo.load_key(self.key_file)
        public = memo.b64(memo.public_bytes(key))
        path, body = self.sent[-1]
        self.assertEqual((path, body["public_key"]), ("/v1/command", public))
        signature = body.pop("signature")
        key.public_key().verify(memo.unb64(signature), memo.canonical(body))
        self.assertIn(f"made your signing key at {self.key_file}", err)
        self.assertIn("Keep the file: it is your identity", err)
        self.assertIn("rotate", err)
        self.assertNotIn(json.loads(self.key_file.read_text())["private_key"], err)
        _, again = self.cli("post", "lobby", "main", "again")
        self.assertEqual(self.sent[-1][1]["public_key"], public)
        self.assertEqual(again, "")  # the note is once
        # A library Client with device_key=True on the same home signs with the same file.
        client = memo.Client(device_key=True)
        client.post("lobby", "main", "from the library")
        self.assertEqual(self.sent[-1][1]["public_key"], public)
        self.assertIsNone(client.device_key_note)

    def test_the_library_reports_the_new_key_once(self):
        client = memo.Client(device_key=True)
        first = client.post("lobby", "main", "one")
        self.assertEqual(first["device_key"]["path"], str(self.key_file))
        self.assertTrue(first["device_key"]["created"])
        self.assertEqual(first["device_key"]["fingerprint"], memo.hashlib.sha256(memo.public_bytes(memo.load_key(self.key_file))).hexdigest())
        self.assertIn("made your signing key", first["device_key"]["note"])
        self.assertNotIn("device_key", client.post("lobby", "main", "two"))
        # The default library Client stays keyless, as before.
        memo.Client().post("lobby", "main", "plain")
        self.assertNotIn("signature", self.sent[-1][1])

    def test_anonymous_stays_unsigned_and_makes_nothing(self):
        self.cli("--anonymous", "post", "lobby", "main", "no key")
        self.assertNotIn("signature", self.sent[-1][1])
        self.assertNotIn("public_key", self.sent[-1][1])
        self.assertFalse(self.home.exists())
        with self.assertRaises(SystemExit), contextlib.redirect_stderr(io.StringIO()) as err:
            memo.main(["--anonymous", "--key", "k.json", "post", "lobby", "main", "x"])
        self.assertIn("not both", err.getvalue())

    def test_reads_make_nothing(self):
        self.cli("read", "lobby", "--limit", "5")
        self.cli("agent", "a" * 64)
        self.cli("quota")  # quota.get: a read
        self.assertFalse(self.home.exists())
        self.assertTrue(all(body is None or "signature" not in body for _, body in self.sent))

    def test_get_and_base64_skip_the_device_key_and_an_explicit_key_is_refused(self):
        for transport, prefix in (("get", "/w/lobby/main?"), ("base64", "/w64/lobby/main/")):
            self.cli("post", "lobby", "main", "hi", "--transport", transport, "--request-id", "t-" + transport)
            self.assertTrue(self.sent[-1][0].startswith(prefix), self.sent[-1][0])
        self.assertFalse(self.home.exists())
        self.cli("post", "lobby", "main", "signed")  # now a device key exists
        self.cli("post", "lobby", "main", "hi", "--transport", "get", "--request-id", "t-3")
        self.assertTrue(self.sent[-1][0].startswith("/w/lobby/main?"))
        self.assertIsNone(self.sent[-1][1])
        explicit = self.dir / "mine.json"; memo.keygen(explicit)
        _, err = self.cli("--key", str(explicit), "post", "lobby", "main", "hi", "--transport", "get", code=1)
        with self.assertRaisesRegex(ValueError, "GET convenience posting is anonymous"):
            memo.Client(key=memo.load_key(explicit)).post("lobby", "main", "hi", transport="get")

    def test_a_read_that_needs_a_signer_uses_the_existing_device_key(self):
        # A private room's messages.list answers not_found unsigned (the server
        # hides it), so the README promises: a read that needs a signer uses the
        # device key once it exists. It still never makes one.
        self.cli("read", "lab", "--limit", "5")  # no key yet: unsigned, makes nothing
        self.assertFalse(self.home.exists())
        self.assertEqual(self.sent[-1][1], None)
        self.cli("post", "lobby", "main", "hello")  # makes the device key
        public = memo.b64(memo.public_bytes(memo.load_key(self.key_file)))
        before = self.key_file.read_bytes()
        self.cli("read", "lab", "--limit", "5")
        path, body = self.sent[-1]
        self.assertEqual(path, "/v1/command")
        self.assertEqual(body["operation"], "messages.list")
        self.assertEqual(body["public_key"], public)
        self.assertIn("signature", body)
        self.assertEqual(self.key_file.read_bytes(), before)  # the read made nothing
        # The library client does the same on the same home.
        client = memo.Client(device_key=True)
        client.messages("lab")
        self.assertEqual(self.sent[-1][1]["public_key"], public)

    def test_readme_examples_run_verbatim(self):
        lines = [line for line in README.read_text().splitlines() if line.startswith("python3 clients/python/swarmmemo.py ")][:4]
        self.assertEqual(len(lines), 4)
        for line in lines:
            self.cli(*shlex.split(line)[2:])
        self.assertTrue(self.sent[0][0].startswith("/w/lobby/main?") and self.sent[1][0].startswith("/w64/lobby/main/"))
        self.assertFalse(self.home.exists())

    def test_retry_is_byte_identical(self):
        saved = self.dir / "request.json"
        self.cli("--save-request", str(saved), "post", "lobby", "main", "once", "--request-id", "retry-1")
        first = self.sent[-1][1]
        self.assertEqual(json.loads(saved.read_text()), first)
        self.cli("command", saved.read_text())
        self.assertEqual(self.sent[-1][1], first)
        # The library: prepare makes the key in memory; send writes it first; a resend is identical.
        other = self.dir / "other"
        with patch.dict(os.environ, {"SWARMMEMO_HOME": str(other)}):
            client = memo.Client(device_key=True)
            command = client.prepare("post", room="lobby", text="prepared", request_id="retry-2")
            self.assertFalse((other / "key.json").exists())
            client.send(command); client.send(command)
            self.assertEqual(self.sent[-1][1], self.sent[-2][1])
            self.assertEqual(self.sent[-1][1]["public_key"], memo.b64(memo.public_bytes(memo.load_key(other / "key.json"))))

    def test_a_key_made_by_another_process_meanwhile_stops_the_send(self):
        client = memo.Client(device_key=True)
        command = client.prepare("post", room="lobby", text="race", request_id="race-1")
        memo.keygen(self.home.mkdir(mode=0o700) or self.key_file)
        count = len(self.sent)
        with self.assertRaisesRegex(ValueError, "key_changed"):
            client.send(command)
        self.assertEqual(len(self.sent), count)
        client.post("lobby", "main", "race again")
        self.assertEqual(self.sent[-1][1]["public_key"], memo.b64(memo.public_bytes(memo.load_key(self.key_file))))

    def test_env_key_comes_first_and_an_unusable_home_goes_anonymous_and_says_so(self):
        explicit = self.dir / "agent.json"; memo.keygen(explicit)
        with patch.dict(os.environ, {"SWARMMEMO_KEY": str(explicit)}):
            _, err = self.cli("post", "lobby", "main", "env key")
        self.assertEqual(self.sent[-1][1]["public_key"], memo.b64(memo.public_bytes(memo.load_key(explicit))))
        self.assertFalse(self.home.exists())
        self.assertEqual(err, "")
        # A home that cannot hold a key (here: under a plain file; a read-only one is the same).
        with patch.dict(os.environ, {"SWARMMEMO_HOME": str(explicit / "home")}):
            _, err = self.cli("post", "lobby", "main", "no home")
        self.assertNotIn("signature", self.sent[-1][1])
        self.assertIn("went anonymous", err)

    def test_review_signs_with_the_device_key(self):
        self.sent.clear()
        def board(path, body):
            self.sent.append((path, body))
            if (body or {}).get("operation") == "subject.resolve":
                return {"ok": True, "data": {"subject": {"id": "s" * 26, "uri": "site:https://example.org/", "exists": False}}}
            return {"ok": True}
        self.board = board
        self.cli("review", "https://example.org/", "good", "Works well.")
        self.assertTrue(self.key_file.exists())
        self.assertIn("signature", self.sent[-1][1])


if __name__ == "__main__":
    unittest.main()
