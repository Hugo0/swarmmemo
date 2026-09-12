import base64
from dataclasses import FrozenInstanceError
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from policy import BridgeError, check_binding, load_profile, read_private


class PolicyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="swarmmemo-mcp-policy-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.state = self.root / "state"; self.state.mkdir(mode=0o700)
        key = bytes(range(32))
        self.value = {"schema": 1, "origin": "https://swarmmemo.com", "service_id": "swarmmemo.com",
                      "public_key": base64.urlsafe_b64encode(key).decode().rstrip("="), "room": "lab",
                      "delegation": {"schema": 1, "grant_id": hashlib.sha256(key).hexdigest(), "generation": "a" * 32},
                      "operations": ["post", "work.claim"], "state_dir": str(self.state)}
        self.path = self.root / "profile.json"
        self.write()

    def write(self, value=None):
        self.path.write_text(json.dumps(self.value if value is None else value)); self.path.chmod(0o600)

    def test_discovery_offline_immutable_and_binding_created_only_explicitly(self):
        profile = load_profile(self.path)
        self.assertEqual(list(self.state.iterdir()), [])
        with self.assertRaises(FrozenInstanceError): profile.room = "elsewhere"
        context = profile.delegation; context["generation"] = "b" * 32
        self.assertEqual(profile.generation, "a" * 32)
        check_binding(profile, create=True)
        binding = self.state / "mcp-policy.json"
        self.assertEqual(binding.stat().st_mode & 0o777, 0o600)
        check_binding(profile)
        for key, value in (("room", "elsewhere"), ("origin", "https://publicbbs.com"), ("operations", ["post"]), ("service_id", "example.org")):
            self.write({**self.value, key: value})
            with self.subTest(key=key), self.assertRaisesRegex(BridgeError, "binding_mismatch"): load_profile(self.path)

    def test_strict_fields_types_context_origin_and_mode(self):
        bad = [{**self.value, "schema": True}, {**self.value, "mode": None}, {**self.value, "approved": True},
               {**self.value, "operations": ["post", "post"]}, {**self.value, "operations": ["delegation.create"]},
               {**self.value, "delegation": {**self.value["delegation"], "schema": True}},
               {**self.value, "delegation": {**self.value["delegation"], "grant_id": "b" * 64}},
               {**self.value, "key_path": "/tmp/never-read"}, {**self.value, "mode": "scoped-send"}]
        bad += [{**self.value, "origin": origin} for origin in ("https://swarmmemo.com/", "https://swarmmemo.com?", "https://@swarmmemo.com", "https://swarmmemo.com/#", "http://remote.example", "https://swarmmemo.com:000443", "https://SWARMMEMO.com", "https://swarmmemo.com\\@evil.example")]
        for value in bad:
            self.write(value)
            with self.subTest(value=value), self.assertRaises(BridgeError): load_profile(self.path)
        self.path.write_text('{"schema":1,"schema":1}')
        with self.assertRaises(BridgeError): load_profile(self.path)
        self.write({**self.value, "origin": "http://127.0.0.1:8102"})
        self.assertEqual(load_profile(self.path).origin, "http://127.0.0.1:8102")

    def test_permissions_symlinks_and_unbound_surviving_queue(self):
        self.path.chmod(0o644)
        with self.assertRaisesRegex(BridgeError, "unsafe_path"): load_profile(self.path)
        self.path.chmod(0o600)
        link = self.root / "linked.json"; link.symlink_to(self.path)
        with self.assertRaisesRegex(BridgeError, "unsafe_path"): load_profile(link)
        self.state.chmod(0o755)
        with self.assertRaisesRegex(BridgeError, "unsafe_path"): load_profile(self.path)
        self.state.chmod(0o700)
        (self.state / "mcp-outbox.sqlite").touch(mode=0o600)
        with self.assertRaisesRegex(BridgeError, "binding_missing"): load_profile(self.path)

    def test_scoped_discovery_does_not_load_key_and_binding_sync_failure_retains_file(self):
        key = self.root / "key.json"; key.write_text("not parsed during discovery"); key.chmod(0o600)
        self.write({**self.value, "mode": "scoped-send", "key_path": str(key)})
        profile = load_profile(self.path)
        self.assertEqual(profile.key_path, key)
        with patch("policy.os.fsync", side_effect=OSError("secret detail")), self.assertRaisesRegex(BridgeError, "unsafe_path"):
            check_binding(profile, create=True)
        self.assertTrue((self.state / "mcp-policy.json").exists())
        check_binding(profile)
        with patch("policy.os.fsync", side_effect=OSError("still not durable")), self.assertRaisesRegex(BridgeError, "unsafe_path"):
            check_binding(profile, create=True)
        with patch("policy.os.fsync", wraps=os.fsync) as sync:
            check_binding(profile, create=True)
            self.assertEqual(sync.call_count, 2)

    def test_raced_nonregular_read_is_nonblocking_and_rejected(self):
        reader, writer = os.pipe()
        self.addCleanup(os.close, writer)
        with patch("policy.os.open", return_value=reader) as opened, self.assertRaisesRegex(BridgeError, "unsafe_path"):
            read_private(self.path)
        self.assertTrue(opened.call_args.args[1] & os.O_NONBLOCK)


if __name__ == "__main__": unittest.main()
