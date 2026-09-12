"""Synthetic-only private reader v3, owner outbox and immutable schema2 tests."""
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import time
import unittest
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "clients/python"))
import swarmmemo as memo
import swarmmemo_outbox as outbox
import swarmmemo_private_transport as transport
import swarmmemo_private_inbox as inbox


def binding(key):
    public = memo.b64(memo.public_bytes(key))
    return {"schema": 2, "type": "private-room-grant-inbox", "origin": "https://example.invalid",
            "service_id": memo.SERVICE, "room": "private-lab", "reader_public_key": public,
            "start_mode": "history", "storage": "metadata-only", "offline_bodies": "deny",
            "private_read": {"schema": 1, "grant_id": hashlib.sha256(memo.public_bytes(key)).hexdigest(), "generation": "a" * 32}}


def capabilities():
    return {"service_id": memo.SERVICE, "canonical_versions": [1, 2, 3], "command_fields": list(memo.FIELDS),
            "private_reads": {"message_get_room_filter": True}, "public_corrections": {"message_read_generation": True},
            "private_read_grants": {"schema": 1, "canonical_version": 3, "context_field": "private_read",
                "room_visibility": "private", "issuer": "room_owner", "operations": ["room.get", "messages.list", "message.get"],
                "transport": "https_json_post", "endpoint": "/v1/command", "room_response": "data.private_room",
                "message_generation": True, "private_mcp": False, "attachment_downloads": False}}


def acknowledgement(command):
    create = command["operation"] == "private_read.create"
    data = memo.strict_json(command["data"])
    child = hashlib.sha256(memo.unb64(command["target"])).hexdigest() if create else command["target"]
    return {"ok": True, "data": {"ack": {"type": "private-room-read-grant-ack", "schema": 1,
        "grant_id": child, "child_id": child, "room": command["room"], "service_id": memo.SERVICE,
        "generation": data["generation"], "grant_generation": data["generation"] if create else "a" * 32,
        "state": "active" if create else "revoked", "accepted_at": 1788566400,
        "expires_at": 1788566400 + command.get("ttl", 86400), "historical_acknowledgement": True}}}


class PrivateReadTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="swarmmemo-private-reader-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.owner = memo.crypto()[0].from_private_bytes(bytes(range(32)))
        self.child = memo.crypto()[0].from_private_bytes(bytes(range(32, 64)))
        self.binding = binding(self.child)
        self.key_path = self.root / "child.json"
        self.key_path.write_bytes(transport.encode({"version": 1, "private_key": memo.b64(bytes(range(32, 64))),
                                                  "public_key": self.binding["reader_public_key"]}))
        self.key_path.chmod(0o600)

    def session(self):
        return transport.ReadSession(self.binding, self.key_path)

    def intent(self, ttl=None):
        return memo.private_read_enrollment_intent(self.binding["reader_public_key"], room="private-lab",
                                                 generation="a" * 32, access_epoch="b" * 32, ttl=ttl)

    def test_shared_v1_v2_v3_and_owner_proof_vectors(self):
        vectors = json.loads((ROOT / "testdata/private_read_vectors.json").read_bytes())
        self.assertEqual(vectors["version"], 1)
        for vector in vectors["vectors"]:
            with self.subTest(name=vector["name"]):
                raw = memo.canonical(vector["command"], vector["service"])
                self.assertEqual(raw.decode(), vector["canonical"])
                key = memo.crypto()[0].from_private_bytes(bytes.fromhex(vector["seed_hex"]))
                self.assertEqual(memo.b64(key.sign(raw)), vector["signature"])
                if "proof" in vector:
                    target = memo.crypto()[0].from_private_bytes(bytes.fromhex(vector["target_seed_hex"]))
                    self.assertEqual(memo.b64(target.sign(raw)), vector["proof"])
        old = json.loads((ROOT / "clients/python/signing-vector.json").read_bytes())
        self.assertEqual(memo.canonical(old["command"]).decode(), old["canonical"])
        self.assertEqual(vectors["vectors"][1]["signature"], "HAO4YsZe-GXmGYSemmy-vuauCkEpcptTUF-E9fl_MMqZljGU6PEORKhk1Anv5fpDe_fMsqVUdw5njcv9N_D3Ag")

    def test_context_strict_and_mixed_never_selects_fallback(self):
        context = self.binding["private_read"]
        invalid = [None, {}, [], "", False, {**context, "schema": True}, {**context, "schema": 1.0},
                   {**context, "grant_id": context["grant_id"].upper()}, {**context, "generation": "a" * 31},
                   {**context, "extra": "value"}]
        for value in invalid:
            with self.subTest(value=value), self.assertRaisesRegex(ValueError, "invalid_private_read_context"):
                memo.canonical({"operation": "room.get", "private_read": value})
        with self.assertRaisesRegex(ValueError, "invalid_private_read_context"):
            memo.canonical({"operation": "room.get", "private_read": context, "delegation": context})
        with self.assertRaises(ValueError): memo.strict_json('{"private_read":{"schema":1,"schema":1}}')
        public = memo.DelegatedClient("https://example.invalid", self.child, grant_id=context["grant_id"],
                                      generation=context["generation"], room="private-lab", operations=["room.get"])
        with self.assertRaisesRegex(ValueError, "delegation_forbidden"):
            public.prepare("room.get", room="private-lab", private_read=context)

    def test_binding_immutable_no_schema1_migration_no_owner_material(self):
        box = inbox.PrivateRoomInbox(self.root / "ledger.sqlite", self.binding)
        box.create(consent_private_metadata=True)
        original = box.binding
        self.binding["private_read"]["generation"] = "b" * 32
        self.assertEqual(box.binding, original)
        with self.assertRaisesRegex(inbox.PrivateInboxError, "binding_mismatch"):
            inbox.PrivateRoomInbox(box.path, self.binding).status()
        ordinary = {k: v for k, v in original.items() if k != "private_read"}
        ordinary.update(schema=1, type="private-room-inbox")
        with self.assertRaisesRegex(inbox.PrivateInboxError, "binding_mismatch"):
            inbox.PrivateRoomInbox(box.path, ordinary).status()
        for changed in ({"schema": True}, {"type": "private-room-inbox"}, {"private_read": None},
                        {"owner_key": "secret"}, {"reader_public_key": memo.b64(memo.public_bytes(self.owner))}):
            with self.assertRaises(transport.PrivateInboxError): transport.binding_validated({**original, **changed})

    def test_capabilities_required_before_signing_and_exact_descriptor(self):
        session = self.session()
        session._fetch = Mock(return_value=capabilities())
        with self.assertRaisesRegex(transport.PrivateInboxError, "capability_check_required"): session.room()
        session._fetch.assert_not_called()
        session.capabilities()
        for field, bad in [("schema", True), ("canonical_version", 2), ("context_field", "delegation"),
                           ("room_visibility", "public"), ("issuer", "member"), ("operations", ["room.get"]),
                           ("transport", "get"), ("endpoint", "/c64"), ("room_response", "room"),
                           ("message_generation", 1), ("private_mcp", True), ("attachment_downloads", True)]:
            value = capabilities(); value["private_read_grants"][field] = bad
            fresh = self.session(); fresh._fetch = Mock(return_value=value)
            with self.assertRaisesRegex(transport.PrivateInboxError, "private_read_capability_required"): fresh.capabilities()
            self.assertFalse(fresh._capabilities_checked)

    def test_minimal_private_room_and_fixed_ten_event_pages(self):
        session = self.session(); session._capabilities_checked = True
        room = {"name": "private-lab", "visibility": "private"}
        session._fetch = Mock(return_value={"ok": True, "data": {"private_room": room}})
        self.assertEqual(session.room(), room)
        for bad in ({"ok": True, "room": room}, {"ok": True, "data": {"private_room": {**room, "owner": "secret"}}},
                    {"ok": True, "data": {"private_room": room, "proof": "secret"}}):
            session._fetch.return_value = bad
            with self.assertRaises(transport.PrivateInboxError): session.room()
        session._fetch.return_value = {"ok": True, "next_cursor": "opaque", "generation": "a" * 32}
        self.assertEqual(session.messages("start")["messages"], [])
        session._fetch.assert_called_with("messages.list", {"cursor": "start", "limit": 10})
        for limit in (1, 100, True, 0):
            with self.assertRaises(transport.PrivateInboxError): session.messages("start", limit=limit)
        for cursor in ("雪" * 1366, "\ud800"):
            session._fetch.reset_mock()
            with self.assertRaisesRegex(transport.PrivateInboxError, "invalid_event_cursor_or_limit"):
                session.messages(cursor)
            session._fetch.assert_not_called()
        session._fetch.return_value["next_cursor"] = "雪" * 1366
        with self.assertRaisesRegex(transport.PrivateInboxError, "invalid_event_response"): session.messages("start")

    def test_worker_signs_v3_exact_scope_without_request_id_or_proof(self):
        calls = []
        def fetch(request):
            calls.append(request)
            return {"status": 200, "body": "e30="}
        for action, args in [("capabilities", {}), ("room.get", {}), ("messages.list", {"cursor": "start", "limit": 10}),
                             ("message.get", {"message_id": "event-1"})]:
            request = {"binding": self.binding, "key_path": str(self.key_path), "action": action, "arguments": args}
            incoming = Mock(buffer=io.BytesIO(transport.encode(request)))
            outgoing = Mock(buffer=io.BytesIO())
            with patch.object(sys, "stdin", incoming), patch.object(sys, "stdout", outgoing), patch.object(transport, "_http", fetch):
                transport._worker()
            self.assertNotIn("error", transport.strict_json(outgoing.buffer.getvalue()))
            current = calls[-1]
            if action == "capabilities":
                self.assertEqual(current.get_method(), "GET"); self.assertIsNone(current.data)
                continue
            self.assertEqual(current.get_method(), "POST")
            command = transport.strict_json(current.data)
            self.assertEqual(command["private_read"], self.binding["private_read"])
            self.assertEqual(set(command), {"operation", "room", "public_key", "signature", "timestamp", "nonce", "private_read"} | set(args))
            self.assertEqual(memo.strict_json(memo.canonical(command))["version"], 3)
            self.child.public_key().verify(memo.unb64(command["signature"]), memo.canonical(command))

    def test_owner_intents_exact_ttl_and_json_only_transport(self):
        self.assertNotIn("ttl", self.intent())
        for ttl in (60, 86400, 604800): self.assertEqual(self.intent(ttl)["ttl"], ttl)
        for ttl in (0, 59, 604801, True, 1.0):
            with self.assertRaises(ValueError): self.intent(ttl)
        client = memo.Client("https://example.invalid", self.owner)
        command = client.prepare_private_read_enrollment(self.child, room="private-lab", generation="a" * 32,
                                                       access_epoch="b" * 32, request_id="owner-create")
        self.assertEqual(memo.strict_json(memo.canonical(command))["version"], 1)
        self.child.public_key().verify(memo.unb64(command["proof"]), memo.canonical(command))
        client._request = Mock(return_value=acknowledgement(command))
        with self.assertRaisesRegex(ValueError, "private_read_https_json_required"): client.send(command, "c64")
        client._request.assert_not_called()

    def test_owner_outbox_exact_lost_ack_retry_and_no_child_mode(self):
        client = memo.Client("https://example.invalid", self.owner)
        box = outbox.Outbox(self.root / "outbox.sqlite", client.base_url, memo.b64(memo.public_bytes(self.owner)))
        box.enqueue("owner-enrollment", self.intent(60))
        sent = []
        def send(command):
            sent.append(copy.deepcopy(command))
            if len(sent) == 1: raise TimeoutError("untrusted network detail")
            return acknowledgement(command)
        client.send = send
        self.assertEqual(box.flush(client, target_key=self.child)[0]["state"], "unresolved")
        box = outbox.Outbox(box.path, client.base_url, memo.b64(memo.public_bytes(self.owner)))
        self.assertEqual(box.flush(client)[0]["state"], "acknowledged")
        self.assertEqual(sent[0], sent[1])
        self.assertEqual(box.flush(client), [])
        with self.assertRaisesRegex(outbox.OutboxError, "unsupported_intent_field"):
            box.enqueue("child-write", {"operation": "post", "text": "must not send", "private_read": self.binding["private_read"]})
        with self.assertRaisesRegex(outbox.OutboxError, "unsupported_targeted_mutation"):
            box.deliver(client, "owner-enrollment", box.inspect("owner-enrollment")["intent_sha256"])
        revoke = memo.private_read_revoke_intent(self.binding["private_read"]["grant_id"], room="private-lab", generation="c" * 32)
        box.enqueue("owner-revoke", revoke)
        self.assertEqual(box.flush(client)[0]["state"], "acknowledged")

    def test_retained_enrollment_proof_rechecked_before_retry_network(self):
        client = memo.Client("https://example.invalid", self.owner)
        box = outbox.Outbox(self.root / "proof-outbox.sqlite", client.base_url, memo.b64(memo.public_bytes(self.owner)))
        box.enqueue("create", self.intent(60))
        client.send = Mock(side_effect=TimeoutError())
        self.assertEqual(box.flush(client, target_key=self.child)[0]["state"], "unresolved")
        with box.database(write=True) as db:
            command = outbox.strict_json(db.execute("SELECT envelope FROM queue").fetchone()[0])
            command["proof"] = memo.b64(b"x" * 64)
            raw = outbox.encoded(command)
            with db: db.execute("UPDATE queue SET envelope=?,envelope_sha256=?", (raw, outbox.sha(raw)))
        result = box.flush(client)[0]
        self.assertEqual(result["state"], "blocked")
        self.assertEqual(result["last_error"], "envelope_signature_invalid")
        self.assertEqual(client.send.call_count, 1)

    def test_schema2_resync_final_fence_never_requests_schema1_page_size(self):
        box = inbox.PrivateRoomInbox(self.root / "resync.sqlite", self.binding)
        box.create(consent_private_metadata=True)
        calls = []
        class Session:
            requests = 0
            def step(self): self.requests += 1
            def capabilities(self): self.step()
            def room(self): self.step()
            def messages(self, cursor, limit=None):
                self.step(); calls.append(limit)
                return {"messages": [], "next_cursor": "opaque", "generation": "a" * 32}
        with patch.object(transport, "ReadSession", return_value=Session()):
            self.assertEqual(box.resync(key_path=self.key_path)["phase"], "ready")
        self.assertEqual(calls, [None, 10])

    def test_ack_exact_fields_current_vs_grant_epoch_and_no_extra_data(self):
        command = memo.sign(self.intent(), self.owner)
        good = acknowledgement(command); outbox.validate_ack(command, good)
        for change in ({"schema": True}, {"historical_acknowledgement": False}, {"room": "other"},
                       {"grant_generation": "c" * 32}, {"accepted_at": True}, {"expires_at": 1},
                       {"proof": "must not persist"}, {"remaining_bytes": 99}):
            bad = copy.deepcopy(good); bad["data"]["ack"].update(change)
            with self.assertRaises(outbox.OutboxError): outbox.validate_ack(command, bad)
        for extra in ({**good, "messages": []}, {"ok": True, "data": {**good["data"], "proof": "secret"}}):
            with self.assertRaises(outbox.OutboxError): outbox.validate_ack(command, extra)
        for field in ("amount", "private_read", "proof"):
            with self.assertRaises(outbox.OutboxError): outbox.intent_bytes("bad", {**self.intent(), field: 0})

    def test_private_control_duplicate_wire_fields_fail_before_ack(self):
        client = memo.Client("https://example.invalid", self.owner)
        command = client.prepare_private_read_enrollment(self.child, room="private-lab", generation="a" * 32,
                                                       access_epoch="b" * 32, request_id="duplicate-wire")
        raw = json.dumps(acknowledgement(command), separators=(",", ":")).encode()
        variants = [
            raw.replace(b'"room":"private-lab"', b'"room":"wrong-room","room":"private-lab"'),
            raw.replace(b'"ok":true', b'"ok":false,"ok":true'),
            raw.replace(b'"data":', b'"data":null,"data":'),
            raw.replace(b'"schema":1', b'"schema":true,"schema":1'),
            raw.replace(b'"ack":', b'"ack":null,"ack":'),
            raw.replace(b'"schema":1', b'"schema":NaN'),
        ]
        for invalid in variants:
            self.assertNotEqual(raw, invalid)
            client.opener = Mock(open=Mock(return_value=io.BytesIO(invalid)))
            with self.assertRaisesRegex(ValueError, "^invalid_private_read_response$"):
                client.send(command)
        client.opener = Mock(open=Mock(return_value=io.BytesIO(raw)))
        outbox.validate_ack(command, client.send(command))
        # Ordinary v1/v2 parsing is intentionally unchanged by this narrow new-control guard.
        for ordinary in ({"operation": "stats"}, {"operation": "room.get", "delegation": self.binding["private_read"]}):
            client.opener = Mock(open=Mock(return_value=io.BytesIO(b'{"ok":false,"ok":true}')))
            self.assertEqual(client.send(ordinary), {"ok": True})

    def test_duplicate_wire_ack_leaves_exact_owner_outbox_retry_unresolved(self):
        client = memo.Client("https://example.invalid", self.owner)
        box = outbox.Outbox(self.root / "duplicate-ack.sqlite", client.base_url, memo.b64(memo.public_bytes(self.owner)))
        box.enqueue("owner-wire-enrollment", self.intent(60))
        envelopes = []
        def response(request, **_):
            envelopes.append(request.data)
            command = memo.strict_json(request.data)
            raw = json.dumps(acknowledgement(command), separators=(",", ":")).encode()
            if len(envelopes) == 1:
                raw = raw.replace(b'"room":"private-lab"', b'"room":"untrusted","room":"private-lab"')
            return io.BytesIO(raw)
        client.opener = Mock(open=response)
        result = box.flush(client, target_key=self.child)[0]
        self.assertEqual(result["state"], "unresolved")
        self.assertEqual(result["last_error"], "delivery_uncertain")
        self.assertEqual(box.flush(client)[0]["state"], "acknowledged")
        self.assertEqual(envelopes[0], envelopes[1])


if __name__ == "__main__": unittest.main()
