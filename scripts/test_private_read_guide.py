"""Execute public schema2 guide snippets against an isolated synthetic server.

Uses SWARMMEMO_PRIVATE_READ_TEST_BINARY, as test_private_read_integration.py does.
Only documented placeholders are substituted; no production URL or key is used.
Run with PYTHONDONTWRITEBYTECODE=1 and python3 -B.
"""
import ast
import os
from pathlib import Path
import re
import shutil
import unittest
from unittest.mock import patch

import test_private_read_integration as integration


GUIDE = Path(__file__).resolve().parents[1] / "clients/python/PRIVATE_INBOX.md"


@unittest.skipUnless(integration.BINARY, "requires explicit disposable private-read Go candidate")
class PrivateReadGuide(unittest.TestCase):
    def setUp(self):
        self.fixture = integration.PrivateReadIntegration()
        self.addCleanup(self.fixture.doCleanups)
        self.fixture.setUp()
        self.text = GUIDE.read_text(encoding="utf-8")
        self.blocks = re.findall(r"^```python\n(.*?)^```", self.text, re.M | re.S)
        self.assertGreaterEqual(len(self.blocks), 8)
        f = self.fixture
        self.staging = f.root / "reader-staging"
        self.reader = f.root / "guide-reader"
        self.staging.mkdir(mode=0o700)
        self.reader.mkdir(mode=0o700)
        self.owner_path = f.operator / "guide-owner.json"
        integration.memo.keygen(self.owner_path)
        self.guide_owner = f.client(integration.memo.load_key(self.owner_path))
        self.guide_owner.command("agent.register")
        self.guide_owner.command("room.create", room="guide-private", visibility="private")
        self.message_id = self.guide_owner.command("post", room="guide-private", text=integration.BODY)["receipt"]["id"]
        self.substitutions = {
            "https://swarmmemo.com": f.origin,
            "private-project": "guide-private",
            "/absolute/operator/owner.json": str(self.owner_path),
            "/absolute/operator/read-grants.sqlite": str(f.operator / "guide-grants.sqlite"),
            "/absolute/reader-staging/child.json": str(self.staging / "child.json"),
            "/absolute/reader/catalog.sqlite": str(self.reader / "catalog.sqlite"),
            "/absolute/reader/child.json": str(self.reader / "child.json"),
        }
        self.proxy_guard = patch.dict(os.environ, {
            "HTTP_PROXY": "", "HTTPS_PROXY": "", "ALL_PROXY": "", "NO_PROXY": "*",
            "http_proxy": "", "https_proxy": "", "all_proxy": "", "no_proxy": "*",
        })
        self.proxy_guard.start()
        self.addCleanup(self.proxy_guard.stop)

    def block(self, identifying_line):
        matches = [block for block in self.blocks if identifying_line in block]
        self.assertEqual(len(matches), 1, "guide snippet must remain unambiguous")
        return matches[0]

    def execute(self, source, namespace):
        for original, local in self.substitutions.items():
            source = source.replace(original, local)
        self.assertNotIn("/absolute/", source, "unresolved guide path")
        self.assertNotIn("REPLACE_WITH", source, "unresolved guide key")
        parsed = ast.parse(source, filename=str(GUIDE))
        for node in ast.walk(parsed):
            if isinstance(node, ast.Constant) and isinstance(node.value, str) and node.value.startswith(("https://", "http://")):
                self.assertEqual(node.value, self.fixture.origin, "guide must not contact external origins")
        exec(compile(parsed, str(GUIDE), "exec"), namespace)

    def test_documented_owner_handoff_two_consumers_and_revoke(self):
        operator = {}
        self.execute(self.block('origin, service, room ='), operator)
        self.assertEqual(operator["saved"]["state"], "acknowledged")
        self.assertEqual(operator["ack"]["grant_id"], operator["child"]["id"])
        self.assertEqual(operator["ack"]["expires_at"] - operator["ack"]["accepted_at"], 86400)

        # The documented handoff is deliberately manual: construct the binding
        # on the operator side, then supply only that object and child key to a
        # distinct reader namespace. This is not an OS-user isolation claim.
        setup = self.block('"type": "private-room-grant-inbox"')
        marker = 'inbox = PrivateRoomInbox('
        self.assertEqual(setup.count(marker), 1)
        binding_code, reader_code = setup.split(marker, 1)
        self.execute(binding_code, operator)
        shutil.copyfile(self.staging / "child.json", self.reader / "child.json")
        (self.reader / "child.json").chmod(0o600)
        reader = {"binding": operator["binding"], "Path": Path}
        self.execute("from swarmmemo_private_inbox import PrivateRoomInbox\n" + marker + reader_code, reader)
        self.assertEqual(reader["status"]["phase"], "ready")
        for forbidden in ("owner", "owner_key", "queue", "saved", "ack", "child_key"):
            self.assertNotIn(forbidden, reader)

        self.execute(self.block('pending = inbox.pending("planner", limit=20)'), reader)
        self.assertEqual(reader["status"]["phase"], "ready")
        self.assertEqual(reader["pending"][0]["message_id"], self.message_id)
        self.execute(self.block('delivery = None'), reader)
        self.assertEqual(reader["delivery"]["message"]["text"], integration.BODY)
        self.execute(self.block('acknowledgement = inbox.ack('), reader)
        self.assertFalse(reader["inbox"].pending("planner"))
        self.assertEqual(len(reader["inbox"].pending("reviewer")), 1)

        self.execute(self.block('queue.enqueue("reader-revoke-1"'), operator)
        self.assertEqual(operator["revoke_status"][0]["state"], "acknowledged")
        saved = operator["queue"].inspect("reader-revoke-1", sensitive=True)
        self.assertEqual(saved["response"]["data"]["ack"]["state"], "revoked")
        self.execute(self.block('status = inbox.resync('), reader)
        self.assertEqual(reader["status"]["phase"], "unavailable")
        with self.assertRaises(integration.PrivateInboxError):
            reader["inbox"].read_current("reviewer", reader["pending"][0]["notification_id"],
                key_path=reader["key_path"], disclose_private_body=True)

        # Public guide execution must not turn the child into a native identity
        # or put author/enrollment proof and plaintext into its local catalog.
        child_id = operator["child"]["id"]
        self.assertEqual(self.fixture.sql("SELECT count(*) FROM identities WHERE id=?", (child_id,)), [(0,)])
        for path in self.reader.iterdir():
            if path.name == "child.json":
                continue
            raw = path.read_bytes()
            for forbidden in (integration.BODY.encode(), b"signed_payload",
                              operator["saved"]["envelope"]["proof"].encode()):
                self.assertNotIn(forbidden, raw)


if __name__ == "__main__":
    unittest.main()
