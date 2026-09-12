"""Explicit public delegation, v1 compatibility and exact durable child intents."""
import contextlib
import copy
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import socket
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import Mock, patch
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "clients/python"))
import swarmmemo as memo
import swarmmemo_outbox as outbox


class DelegationClientTests(unittest.TestCase):
    def setUp(self):
        self.key = memo.crypto()[0].from_private_bytes(bytes(range(32)))
        self.public = memo.b64(memo.public_bytes(self.key))
        self.child_id = hashlib.sha256(memo.public_bytes(self.key)).hexdigest()
        self.context = {"schema": 1, "grant_id": self.child_id, "generation": "a" * 32}
        self.operations = ["post", "messages.list", "work.claim", "work.renew", "work.submit"]
        self.client = memo.DelegatedClient("https://example.org", self.key, grant_id=self.child_id, generation="a" * 32,
                                           room="lab", operations=self.operations, timeout=10)

    def post(self):
        return {"operation": "post", "room": "lab", "visibility": "public", "text": "delegated fixture", "delegation": dict(self.context)}

    def test_v1_unchanged_v2_nested_order_and_go_vector(self):
        vector = json.loads((ROOT / "clients/python/signing-vector.json").read_bytes())
        self.assertEqual(memo.canonical(vector["command"]), vector["canonical"].encode())
        command = {**vector["command"], "delegation": dict(reversed(list(self.context.items())))}
        expected = vector["canonical"].replace('"version":1', '"version":2', 1)[:-2] + ',"delegation":' + json.dumps(self.context, separators=(",", ":")) + '}}'
        self.assertEqual(memo.canonical(command), expected.encode())
        signed = memo.sign(command, self.key)
        self.assertEqual(signed["signature"], "HAO4YsZe-GXmGYSemmy-vuauCkEpcptTUF-E9fl_MMqZljGU6PEORKhk1Anv5fpDe_fMsqVUdw5njcv9N_D3Ag")
        memo.crypto()[1].from_public_bytes(memo.public_bytes(self.key)).verify(memo.unb64(signed["signature"]), expected.encode())
        binary = os.environ.get("SWARMMEMO_DELEGATION_TEST_BINARY")
        if binary:
            actual = subprocess.run([binary, "canonical"], input=json.dumps(command).encode(), capture_output=True, check=True, timeout=10)
            self.assertEqual(actual.stdout, expected.encode())

    def test_context_strict_presence_types_unknowns_and_duplicates(self):
        invalid = [None, {}, "", False, [], {**self.context, "schema": True}, {**self.context, "schema": 1.0},
                   {**self.context, "grant_id": self.child_id.upper()}, {**self.context, "generation": "a" * 31},
                   {**self.context, "unknown": 1}, {"Schema": 1, "grant_id": self.child_id, "generation": "a" * 32}]
        for context in invalid:
            with self.subTest(context=context), self.assertRaises(ValueError): memo.canonical({"operation": "post", "delegation": context})
        for raw in ('{"delegation":{"schema":1,"schema":1}}', '{"delegation":null,"delegation":{}}'):
            with self.assertRaisesRegex(ValueError, "duplicate_json_field"): memo.strict_json(raw)

    def test_wrapper_copies_context_and_requires_original_bound_key_room_scope(self):
        command = self.client.prepare(**self.post())
        self.assertEqual(command["delegation"], self.context)
        exposed = self.client.delegation; exposed["generation"] = "b" * 32
        self.operations.clear()
        self.assertEqual(self.client.delegation, self.context)
        self.client._request = Mock(return_value={"ok": True})
        self.client.send(command)
        for changed in ({**command, "delegation": None}, {k: v for k, v in command.items() if k != "delegation"},
                        {**command, "delegation": {**self.context, "generation": "b" * 32}}, {**command, "public_key": "different"}):
            with self.assertRaises(ValueError): self.client.send(changed)
        self.assertEqual(self.client._request.call_count, 1)
        for fields in ({"room": "other"}, {"visibility": "private"}, {"visibility": ""}, {"handle": "root"}, {"attachments": ["asset"]}):
            with self.assertRaises(ValueError): self.client.prepare(**{**self.post(), **fields})
        for operation in ("delegation.create", "work.accept", "agent.register", "blob.put"):
            with self.assertRaisesRegex(ValueError, "delegation_forbidden"): self.client.prepare(operation)
        self.client.base_url = "https://other.example"
        with self.assertRaisesRegex(ValueError, "binding_mismatch"): self.client.send(command)

    def test_own_inactive_status_preserves_old_context_and_work_generation(self):
        status = self.client.prepare("delegation.get", target=self.child_id)
        self.assertEqual(status["delegation"], self.context)
        with self.assertRaises(ValueError): self.client.prepare("delegation.get", target="f" * 64)
        with self.assertRaises(ValueError): self.client.prepare("messages.list")
        with self.assertRaises(ValueError): self.client.prepare("work.claim", message_id="b" * 32, ttl=60, data='{"schema":1,"generation":"' + "b" * 32 + '"}')
        self.client._request = Mock(side_effect=memo.APIError(403, "delegation_inactive", "untrusted error"))
        command = self.client.prepare(**self.post())
        original = copy.deepcopy(command)
        with self.assertRaises(memo.APIError): self.client.send(command)
        self.assertEqual(command, original)
        self.assertEqual(self.client.prepare("delegation.get", target=self.child_id)["delegation"], self.context)
        self.assertEqual(self.client._request.call_count, 1)

    def test_work_schema_is_exact_before_signing_or_network(self):
        self.client.opener.open = Mock(side_effect=AssertionError("network forbidden"))
        valid = {"schema": 1, "generation": self.context["generation"]}
        for operation in ("work.claim", "work.renew", "work.submit"):
            for data in ({"generation": valid["generation"]}, {**valid, "schema": True},
                         {**valid, "schema": 1.0}, {**valid, "schema": 2}, {**valid, "extra": 1}):
                with self.subTest(operation=operation, data=data), self.assertRaisesRegex(ValueError, "delegation_generation_mismatch"):
                    self.client.prepare(operation, message_id="b" * 32, data=json.dumps(data))
            prepared = self.client.prepare(operation, message_id="b" * 32, data=json.dumps(valid))
            self.assertEqual(json.loads(prepared["data"]), valid)
        self.client.opener.open.assert_not_called()

    def test_cleared_key_and_raw_convenience_never_fall_back_to_anonymous(self):
        self.client.opener.open = Mock(side_effect=AssertionError("network forbidden"))
        with self.assertRaises(ValueError): self.client._request("/api/messages?room=other")
        self.client.key = None
        with self.assertRaises(ValueError): self.client.messages(room="lab")
        with self.assertRaises(ValueError): self.client.post("lab", "main", "must not post", transport="get")
        self.client.opener.open.assert_not_called()

    def test_enrollment_prepares_both_possession_signatures_without_network(self):
        parent_key = memo.crypto()[0].generate()
        parent = memo.Client("https://example.org", parent_key)
        parent.send = Mock(side_effect=AssertionError("prepare cannot send"))
        command = parent.prepare_enrollment(self.key, room="lab", ttl=120, amount=10000, generation="a" * 32, operations=["post"])
        self.assertNotIn("delegation", command)
        self.assertEqual(json.loads(command["data"])["disclosure"], "public")
        for public_key, field in ((memo.public_bytes(parent_key), "signature"), (memo.public_bytes(self.key), "proof")):
            memo.crypto()[1].from_public_bytes(public_key).verify(memo.unb64(command[field]), memo.canonical(command))
        parent.send.assert_not_called()


class DelegationOutboxTests(unittest.TestCase):
    post = DelegationClientTests.post

    def setUp(self):
        DelegationClientTests.setUp(self)
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / "outbox.sqlite"
        self.queue = outbox.Outbox(self.path, self.client.base_url, self.public, delegation=self.context)

    def ack(self, command):
        return {"ok": True, "receipt": {"id": "f" * 32, "sha256": outbox.sha(command["text"].encode()), "accepted_at": 100}}

    def test_context_durable_binding_exact_retry_and_denial_never_resigns(self):
        self.queue.enqueue("child-intent", self.post())
        self.client.send = Mock(side_effect=OSError("lost response"))
        self.assertEqual(self.queue.flush(self.client)[0]["state"], "unresolved")
        envelope = self.queue.inspect("child-intent", sensitive=True)["envelope"]
        self.assertEqual(envelope["delegation"], self.context)
        self.client.send.side_effect = memo.APIError(403, "delegation_inactive", "never retain token")
        self.assertEqual(self.queue.flush(self.client)[0]["state"], "blocked")
        self.assertEqual(self.client.send.call_args.args[0], envelope)
        self.assertEqual(self.queue.flush(self.client)[0]["state"], "blocked")
        self.assertEqual(self.client.send.call_count, 2)
        self.client.send.side_effect = self.ack
        with patch.object(self.client, "prepare", side_effect=AssertionError("cannot re-sign")):
            self.assertEqual(self.queue.flush(self.client, retry_blocked=True)[0]["state"], "acknowledged")
        self.assertEqual(self.client.send.call_args.args[0], envelope)
        self.assertNotIn("never retain token", json.dumps(self.queue.status()))
        with self.assertRaises(outbox.OutboxError): outbox.Outbox(self.path, self.client.base_url, self.public).status()
        with self.assertRaises(outbox.OutboxError): outbox.Outbox(self.path, self.client.base_url, self.public, delegation={**self.context, "generation": "b" * 32}).status()
        with self.assertRaises(outbox.OutboxError): self.queue.flush(memo.Client(self.client.base_url, self.key))
        with self.assertRaises(outbox.OutboxError): self.queue.enqueue("missing", {k: v for k, v in self.post().items() if k != "delegation"})

    def test_root_v1_queue_read_and_transactional_schema_upgrade_preserve_intents(self):
        legacy = outbox.SCHEMA.replace(",delegation TEXT NOT NULL DEFAULT ''", "").replace("user_version=2", "user_version=1")
        with contextlib.closing(sqlite3.connect(self.path)) as db:
            db.executescript(legacy)
            with db: db.execute("INSERT INTO binding VALUES(?,?,?)", ("swarmmemo.com", self.client.base_url, self.public))
        self.path.chmod(0o600)
        root = outbox.Outbox(self.path, self.client.base_url, self.public)
        self.assertEqual(root.status()["items"], [])
        with contextlib.closing(sqlite3.connect(self.path)) as db:
            self.assertEqual(db.execute("PRAGMA user_version").fetchone()[0], 1)
        intent = {"operation": "post", "room": "lab", "text": "v1 unchanged"}
        root.enqueue("root", intent)
        with contextlib.closing(sqlite3.connect(self.path)) as db:
            self.assertEqual(db.execute("PRAGMA user_version").fetchone()[0], 2)
        client = memo.Client(self.client.base_url, self.key, timeout=10); client.send = Mock(side_effect=self.ack)
        self.assertEqual(root.flush(client)[0]["state"], "acknowledged")
        self.assertNotIn("delegation", client.send.call_args.args[0])
        self.assertEqual(root.inspect("root", sensitive=True)["intent"], intent)

    def enrollment(self):
        parent_key = memo.crypto()[0].generate()
        parent = memo.Client(self.client.base_url, parent_key, timeout=10)
        queue = outbox.Outbox(self.path, parent.base_url, memo.b64(memo.public_bytes(parent_key)))
        intent = {"operation": "delegation.create", "room": "lab", "target": self.public, "ttl": 120, "amount": 10000,
                  "data": json.dumps({"schema": 1, "generation": "a" * 32, "operations": ["post"], "disclosure": "public"})}
        queue.enqueue("enroll", intent)
        ack = {"ok": True, "data": {"ack": {"grant_id": self.child_id, "child_id": self.child_id, "generation": "a" * 32,
               "service_id": "swarmmemo.com", "state": "active", "accepted_at": 100, "expires_at": 220, "ceiling_bytes": 10000}}}
        return parent, queue, intent, ack

    def test_enrollment_proof_retained_verified_and_retry_needs_no_target_key(self):
        parent, queue, intent, ack = self.enrollment()
        parent.send = Mock(side_effect=OSError("accepted response lost"))
        self.assertEqual(queue.flush(parent, target_key=self.key)[0]["state"], "unresolved")
        envelope = queue.inspect("enroll", sensitive=True)["envelope"]
        memo.crypto()[1].from_public_bytes(memo.public_bytes(self.key)).verify(memo.unb64(envelope["proof"]), memo.canonical(envelope))
        parent.send.side_effect = lambda command: ack
        with patch.object(parent, "prepare", side_effect=AssertionError("cannot re-sign enrollment")):
            self.assertEqual(queue.flush(parent)[0]["state"], "acknowledged")
        self.assertEqual(parent.send.call_args.args[0], envelope)
        for field in ("grant_id", "child_id", "generation", "service_id", "state", "expires_at", "ceiling_bytes"):
            wrong = copy.deepcopy(ack); wrong["data"]["ack"][field] = "wrong"
            with self.subTest(field=field), self.assertRaises(outbox.OutboxError): outbox.validate_ack(envelope, wrong)
        for extra in ({"unexpected": "private echo"}, {"data": {**ack["data"], "text": "private echo"}}):
            with self.assertRaises(outbox.OutboxError): outbox.validate_ack(envelope, {**ack, **extra})

    def test_tampered_enrollment_proof_blocks_before_network_even_rehashed(self):
        parent, queue, _, _ = self.enrollment()
        parent.send = Mock(side_effect=OSError("lost"))
        queue.flush(parent, target_key=self.key)
        envelope = queue.inspect("enroll", sensitive=True)["envelope"]
        envelope["proof"] = memo.b64(bytes(64)); raw = outbox.encoded(envelope)
        with contextlib.closing(sqlite3.connect(self.path)) as db, db:
            db.execute("UPDATE queue SET envelope=?,envelope_sha256=?", (raw, outbox.sha(raw)))
        parent.send.reset_mock()
        result = queue.flush(parent)
        self.assertEqual(result[0]["state"], "blocked")
        self.assertEqual(result[0]["last_error"], "envelope_signature_invalid")
        parent.send.assert_not_called()

    def test_revocation_ack_is_bounded_and_allows_expired_grant(self):
        command = {"operation": "delegation.revoke", "target": self.child_id, "data": '{"schema":1,"generation":"' + "b" * 32 + '"}'}
        result = {"ok": True, "data": {"ack": {"grant_id": self.child_id, "child_id": self.child_id, "generation": "b" * 32,
                  "service_id": "swarmmemo.com", "state": "revoked", "accepted_at": 1000, "expires_at": 200, "ceiling_bytes": 10000}}}
        outbox.validate_ack(command, result)
        wrong = copy.deepcopy(result); wrong["data"]["ack"]["ceiling_bytes"] = True
        with self.assertRaises(outbox.OutboxError): outbox.validate_ack(command, wrong)


@unittest.skipUnless(os.environ.get("SWARMMEMO_DELEGATION_TEST_BINARY"), "explicit delegation-capable local Go binary required")
class DelegationGoIntegration(unittest.TestCase):
    def test_real_enrollment_child_outbox_revocation_and_inactive_status(self):
        with tempfile.TemporaryDirectory(prefix="swarmmemo-delegation-client-") as directory:
            local = Path(directory)
            with socket.socket() as reservation:
                reservation.bind(("127.0.0.1", 0)); port = reservation.getsockname()[1]
            origin = "http://127.0.0.1:" + str(port)
            env = {**os.environ, "DATA_DIR": str(local / "server"), "LISTEN_ADDR": "127.0.0.1:" + str(port),
                   "ALLOW_INSECURE_LOCAL": "true", "TRUST_LOOPBACK_PROXY": "false", "ADMIN_TOKEN_FILE": "", "SERVICE_ID": memo.SERVICE}
            process = subprocess.Popen([os.environ["SWARMMEMO_DELEGATION_TEST_BINARY"], "serve"], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                for _ in range(100):
                    try:
                        with urllib.request.urlopen(origin + "/health", timeout=1): break
                    except OSError:
                        self.assertIsNone(process.poll()); time.sleep(.05)
                else: self.fail("local server did not start")
                parent_key, child_key = memo.crypto()[0].generate(), memo.crypto()[0].generate()
                parent = memo.Client(origin, parent_key, timeout=10)
                public = memo.b64(memo.public_bytes(child_key)); child_id = hashlib.sha256(memo.public_bytes(child_key)).hexdigest()
                parent.command("room.create", room="delegation-lab", visibility="public")
                generation = memo.Client(origin)._request("/api/changes?after=-1")["generation"]
                intent = {"operation": "delegation.create", "room": "delegation-lab", "target": public, "ttl": 600, "amount": 65536,
                          "data": json.dumps({"schema": 1, "generation": generation, "operations": ["post", "messages.list"], "disclosure": "public"})}
                parent_queue = outbox.Outbox(local / "parent.sqlite", origin, memo.b64(memo.public_bytes(parent_key)))
                parent_queue.enqueue("enroll", intent)
                self.assertEqual(parent_queue.flush(parent, target_key=child_key)[0]["state"], "acknowledged")
                enrollment = parent_queue.inspect("enroll", sensitive=True)["envelope"]
                parent.send(enrollment)  # Exact parent/child-proof retry.
                with self.assertRaises(memo.APIError): parent.send({**enrollment, "proof": memo.b64(bytes(64))})
                child = memo.DelegatedClient(origin, child_key, grant_id=child_id, generation=generation,
                                              room="delegation-lab", operations=["post", "messages.list"], timeout=10)
                queue = outbox.Outbox(local / "child.sqlite", origin, public, delegation=child.delegation)
                queue.enqueue("child-post", {"operation": "post", "room": "delegation-lab", "visibility": "public",
                                             "kind": "simulation", "text": "Operator-owned delegated integration fixture", "delegation": child.delegation})
                send = child.send
                def lost(command):
                    send(command); raise OSError("response lost after actual acceptance")
                with patch.object(child, "send", side_effect=lost):
                    self.assertEqual(queue.flush(child)[0]["state"], "unresolved")
                original = queue.inspect("child-post", sensitive=True)["envelope"]
                parent_queue.enqueue("revoke", {"operation": "delegation.revoke", "target": child_id,
                                                "data": json.dumps({"schema": 1, "generation": generation})})
                self.assertEqual(parent_queue.flush(parent)[0]["state"], "acknowledged")
                with patch.object(child, "prepare", side_effect=AssertionError("never re-sign exact retry")):
                    self.assertEqual(queue.flush(child)[0]["state"], "acknowledged")
                self.assertEqual(queue.inspect("child-post", sensitive=True)["envelope"], original)
                status = child.command("delegation.get", target=child_id)["data"]["delegation"]
                self.assertEqual(status["state"], "revoked"); self.assertEqual(status["generation"], generation)
                self.assertEqual(set(status), {"grant_id", "generation", "service_id", "state", "created_at", "expires_at", "ceiling_bytes", "used_bytes", "remaining_bytes"})
                with self.assertRaises(memo.APIError):
                    child.command("post", room="delegation-lab", visibility="public", kind="simulation", text="must not be accepted")
                events = memo.Client(origin).messages(room="delegation-lab")["messages"]
                self.assertEqual(len(events), 1)
                self.assertEqual(events[0]["author"], child_id); self.assertEqual(events[0]["delegation_id"], child_id)
                self.assertEqual(events[0]["signed_payload"], memo.canonical(original).decode())
            finally:
                process.terminate()
                try: process.wait(timeout=5)
                except subprocess.TimeoutExpired: process.kill(); process.wait(timeout=5)


if __name__ == "__main__": unittest.main()
