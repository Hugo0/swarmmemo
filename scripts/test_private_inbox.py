"""Synthetic metadata-ledger fixtures; fake transport is patched only by tests."""
import copy
import fcntl
import hashlib
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "clients/python"))
import swarmmemo as memo
import swarmmemo_private_inbox as inbox
import swarmmemo_private_transport as transport

CANARY = "private-body-CANARY-never-persist-☃"
GENERATION = "a" * 32


class Source:
    def __init__(self, case):
        self.case = case
        self.messages = []
        self.generation = GENERATION
        self.allowed = True
        self.auth = True
        self.calls = []
        self.hook = None
        self.page_limit = 100
        self.cursors = {"start": 0, "": None}

    def session(self, binding, key_path, max_requests=10, deadline_seconds=30):
        source = self
        class Session:
            requests = 0
            def step(self, action):
                if self.requests >= max_requests:
                    raise transport.PrivateInboxError("request_budget")
                self.requests += 1
                source.calls.append(action)
                if source.hook:
                    source.hook(action)
                if not source.auth:
                    raise transport.PrivateInboxError("reauth_required")
            def capabilities(self):
                self.step("capabilities")
            def room(self):
                self.step("room")
                if not source.allowed:
                    raise transport.PrivateInboxError("scope_unavailable")
            def messages(self, cursor, limit=100):
                self.step("messages")
                if not source.allowed:
                    raise transport.PrivateInboxError("scope_unavailable")
                boundary = source.cursors.get(cursor, 0)
                count = min(limit, source.page_limit)
                records = source.messages[-count:] if boundary is None else [e for e in source.messages if e["sequence"] > boundary][:count]
                end = records[-1]["sequence"] if records else (boundary or 0)
                token = "opaque-" + str(end)
                source.cursors[token] = end
                return {"messages": copy.deepcopy(records), "next_cursor": token if records or cursor in ("", "start") else cursor,
                        "generation": source.generation}
            def event(self, message_id):
                self.step("event:" + message_id)
                if not source.allowed:
                    return None
                item = next((e for e in source.messages if e["id"] == message_id), None)
                return None if item is None else {"messages": [copy.deepcopy(item)], "next_cursor": "single-opaque", "generation": source.generation}
        return Session()


class LedgerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="swarmmemo-private-ledger-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.key = memo.crypto()[0].from_private_bytes(bytes(range(32)))
        self.public = memo.b64(memo.public_bytes(self.key))
        self.binding = {"schema": 1, "type": "private-room-inbox", "origin": "https://unused.invalid",
                        "service_id": "swarmmemo.com", "room": "private-lab", "reader_public_key": self.public,
                        "start_mode": "history", "storage": "metadata-only", "offline_bodies": "deny"}
        self.path = self.root / "ledger.sqlite"
        self.box = inbox.PrivateRoomInbox(self.path, self.binding)
        self.source = Source(self)
        fixture = patch.object(transport, "ReadSession", side_effect=self.source.session)
        self.mock_session = fixture.start()
        self.addCleanup(fixture.stop)

    def create(self):
        self.box.create(consent_private_metadata=True)
        self.box.add_consumer("planner")

    def event(self, number=1, *, attachments=False, text=CANARY):
        fields = {"operation": "post", "room": self.binding["room"], "page": "main", "kind": "note",
                  "text": text, "request_id": "fixture-" + str(number)}
        parts = []
        if attachments:
            fields["attachments"] = ["blob-" + str(number)]
            parts = [{"id": fields["attachments"][0], "room": self.binding["room"], "filename": CANARY,
                      "media_type": "text/plain", "sha256": "c" * 64, "size": 1,
                      "created_at": 1, "expires_at": 9999999999, "expired": False, "deleted": False}]
        command = memo.sign(fields, self.key)
        event = {"id": "event-" + str(number), "sequence": number, "room": self.binding["room"], "page": "main",
                 "kind": "note", "text": text, "author": hashlib.sha256(memo.public_bytes(self.key)).hexdigest(),
                 "public_key": self.public, "created_at": 100 + number, "sha256": hashlib.sha256(text.encode()).hexdigest(),
                 "signature": command["signature"], "signed_payload": memo.canonical(command).decode(),
                 "type": "message", "hidden": False, "visibility": "private", "archive_eligible": False}
        if parts:
            event["attachments"] = parts
        return event

    @staticmethod
    def hide(event):
        value = copy.deepcopy(event)
        value.update(type="tombstone", hidden=True, text="", signature="", signed_payload="", reason=CANARY)
        value.pop("attachments", None)
        return value

    def poll(self, **kwargs):
        return self.box.poll(key_path=self.root / "never-loaded-key.json", **kwargs)

    def resync(self, **kwargs):
        return self.box.resync(key_path=self.root / "never-loaded-key.json", **kwargs)

    def complete_resync(self, **kwargs):
        for _ in range(30):
            result = self.resync(**kwargs)
            if result["phase"] == "ready":
                return result
            self.assertEqual(result["last_error"], "request_budget", result)
        self.fail("bounded resync did not finish")

    def row(self, table="checkpoint"):
        with self.box.database() as db:
            return dict(db.execute("SELECT * FROM " + table).fetchone())

    def test_consent_offline_binding_and_no_implicit_consumer(self):
        self.assertEqual(self.box.status()["phase"], "not_created")
        self.assertEqual(list(self.root.iterdir()), [])
        with self.assertRaisesRegex(inbox.PrivateInboxError, "consent"):
            self.box.create()
        self.create()
        self.assertEqual(self.box.pending("planner"), [])
        with self.assertRaisesRegex(inbox.PrivateInboxError, "unknown_consumer"):
            self.box.pending("intruder")
        self.mock_session.assert_not_called()
        changed = {**self.binding, "room": "other-room"}
        with self.assertRaisesRegex(inbox.PrivateInboxError, "binding_mismatch"):
            inbox.PrivateRoomInbox(self.path, changed).status()
        original = self.box.binding
        original["room"] = "changed"
        self.assertEqual(self.box.binding, self.binding)

    def test_two_consumers_restart_ack_and_body_never_persisted(self):
        self.create()
        self.source.messages = [self.event(attachments=True)]
        self.assertEqual(self.poll()["phase"], "ready")
        item = self.box.pending("planner")[0]
        self.box.add_consumer("reviewer")
        self.assertEqual(self.box.pending("reviewer")[0], item)
        with self.assertRaisesRegex(inbox.PrivateInboxError, "consent"):
            self.box.read_current("planner", item["notification_id"], key_path=self.root / "key")
        read = self.box.read_current("planner", item["notification_id"], key_path=self.root / "key", disclose_private_body=True)
        self.assertEqual(read["message"]["text"], CANARY)
        self.box.ack("planner", item["notification_id"], item["snapshot_digest"])
        self.box = inbox.PrivateRoomInbox(self.path, self.binding)
        self.assertEqual(self.box.pending("planner"), [])
        self.assertEqual(len(self.box.pending("reviewer")), 1)
        self.assertTrue(self.box.ack("planner", item["notification_id"], item["snapshot_digest"])["duplicate"])
        for path in self.root.iterdir():
            data = path.read_bytes()
            self.assertNotIn(CANARY.encode(), data)
            self.assertNotIn(self.source.messages[0]["signature"].encode(), data)
            self.assertNotIn(b"signed_payload", data)

    def test_attachment_changes_supersede_and_digest_reversion_is_new_notification(self):
        self.create()
        self.source.messages = [self.event(attachments=True)]
        self.poll()
        first = self.box.pending("planner")[0]
        self.source.messages[0]["attachments"][0]["deleted"] = True
        with self.assertRaisesRegex(inbox.PrivateInboxError, "notification_superseded"):
            self.box.read_current("planner", first["notification_id"], key_path=self.root / "key", disclose_private_body=True)
        second = self.box.pending("planner")[0]
        self.assertNotEqual(first["snapshot_digest"], second["snapshot_digest"])
        self.source.messages[0]["attachments"][0]["deleted"] = False
        self.poll()
        third = self.box.pending("planner")[0]
        self.assertEqual(first["snapshot_digest"], third["snapshot_digest"])
        self.assertGreater(third["notification_id"], second["notification_id"])
        with self.assertRaisesRegex(inbox.PrivateInboxError, "superseded"):
            self.box.ack("planner", first["notification_id"], first["snapshot_digest"])

    def test_tombstone_latch_survives_unhide_resync_and_first_observed_tombstone(self):
        self.create()
        original = self.event(attachments=True)
        self.source.messages = [original]
        self.poll()
        item = self.box.pending("planner")[0]
        self.box.ack("planner", item["notification_id"], item["snapshot_digest"])
        self.source.messages = [self.hide(original)]
        self.poll()
        removal = self.box.pending("planner")[0]
        self.assertEqual(removal["state"], "tombstoned")
        self.source.messages = [original, self.hide(self.event(2, attachments=True))]
        self.complete_resync()
        self.source.messages[1] = self.event(2, attachments=True)
        self.complete_resync()
        self.assertTrue(all(i["state"] == "tombstoned" for i in self.box.pending("planner")))
        self.assertEqual(self.box.pending("planner")[0]["notification_id"], removal["notification_id"])

    def test_observed_membership_denial_is_sticky_until_explicit_resync(self):
        self.create()
        self.source.messages = [self.event()]
        self.poll()
        self.source.allowed = False
        self.assertEqual(self.poll()["phase"], "unavailable")
        self.assertEqual(self.box.pending("planner"), [])
        self.source.allowed = True
        calls = len(self.source.calls)
        with self.assertRaisesRegex(inbox.PrivateInboxError, "unavailable"):
            self.poll()
        self.assertEqual(len(self.source.calls), calls)
        self.complete_resync()
        self.assertEqual(len(self.box.pending("planner")), 1)

    def test_generation_restore_preserves_ack_and_abandons_mixed_pass(self):
        self.create()
        self.source.messages = [self.event()]
        self.poll()
        item = self.box.pending("planner")[0]
        self.box.ack("planner", item["notification_id"], item["snapshot_digest"])
        self.source.generation = "b" * 32
        self.assertEqual(self.poll()["phase"], "resync_required")
        self.complete_resync()
        self.assertEqual(self.box.pending("planner"), [])
        self.source.messages = []
        count = 0
        def change_at_final_fence(action):
            nonlocal count
            if action == "messages":
                count += 1
                if count == 2:
                    self.source.generation = "c" * 32
        self.source.hook = change_at_final_fence
        result = self.resync()
        self.assertEqual(result["phase"], "resync_required")
        self.assertFalse(result["resync_active"])
        self.source.hook = None
        self.complete_resync()
        self.assertEqual(self.box.pending("planner")[0]["state"], "unavailable")

    def test_bounded_resync_continues_persisted_round_and_blocks_content(self):
        self.create()
        self.source.page_limit = 1
        self.source.messages = [self.event(i) for i in range(1, 7)]
        result = self.resync(max_requests=5)
        self.assertEqual(result["phase"], "reconciling")
        initial_round = self.row()["round"]
        self.assertEqual(self.box.pending("planner"), [])
        self.box = inbox.PrivateRoomInbox(self.path, self.binding)
        self.complete_resync(max_requests=5)
        self.assertEqual(self.row()["round"], initial_round)
        self.assertEqual(len(self.box.pending("planner")), 6)

    def test_staged_digest_divergence_then_reversion_never_revives_historical_ack(self):
        self.create()
        original = self.event(attachments=True)
        self.source.messages = [original]
        self.poll()
        initial = self.box.pending("planner")[0]
        self.box.ack("planner", initial["notification_id"], initial["snapshot_digest"])
        changed = copy.deepcopy(original)
        changed["attachments"][0]["deleted"] = True
        self.source.page_limit = 1
        self.source.messages = [changed] + [self.event(i) for i in range(2, 8)]
        self.assertEqual(self.resync(max_requests=5)["phase"], "reconciling")
        self.source.generation = "b" * 32
        self.source.messages[0] = original
        self.assertEqual(self.resync(max_requests=5)["phase"], "resync_required")
        self.complete_resync(max_requests=5)
        returned = next(i for i in self.box.pending("planner") if i["message_id"] == original["id"])
        self.assertEqual(returned["snapshot_digest"], initial["snapshot_digest"])
        self.assertGreater(returned["notification_id"], initial["notification_id"])
        self.assertEqual(returned["kind"], "correction")

    def test_immutable_drift_never_rebinds_existing_event(self):
        self.create()
        self.source.messages = [self.event()]
        self.poll()
        original = self.row("messages")["immutable"]
        self.source.messages = [self.event(text="different signed content")]
        self.assertEqual(self.poll()["last_error"], "source_identity_changed")
        self.assertEqual(self.resync()["last_error"], "source_identity_changed")
        self.assertEqual(self.row("messages")["immutable"], original)
        self.assertEqual(self.box.pending("planner"), [])

    def test_capacity_does_not_rollback_known_removal_or_advance_page(self):
        self.create()
        original = self.event()
        self.source.messages = [original]
        self.poll()
        before = self.row()["cursor"]
        self.source.messages = [self.hide(original), self.event(2)]
        with patch.object(inbox, "MAX_EVENTS", 1):
            result = self.poll(max_requests=5)
        self.assertEqual(result["last_error"], "event_capacity")
        self.assertEqual(self.row()["cursor"], before)
        self.assertEqual(self.box.pending("planner")[0]["state"], "tombstoned")

    def test_reserved_removal_and_acks_survive_full_notification_budget(self):
        self.create()
        self.box.add_consumer("reviewer")
        original = self.event()
        self.source.messages = [original]
        with patch.object(inbox, "MAX_NOTIFICATIONS", 3):
            self.assertEqual(self.poll()["phase"], "ready")
            initial = self.box.pending("planner")[0]
            self.box.ack("planner", initial["notification_id"], initial["snapshot_digest"])
            self.source.messages = []
            self.assertEqual(self.poll(max_requests=5)["last_error"], None)
            missing = self.box.pending("planner")[0]
            self.assertEqual(missing["state"], "unavailable")
            self.source.messages = [self.hide(original)]
            self.assertEqual(self.poll(max_requests=5)["last_error"], None)
            removal = self.box.pending("planner")[0]
            self.assertEqual(removal["state"], "tombstoned")
            for consumer in ("planner", "reviewer"):
                self.box.ack(consumer, removal["notification_id"], removal["snapshot_digest"])
            self.assertEqual(self.box.status()["notifications"], 3)

    def test_invalid_response_never_advances_and_wrong_event_never_returns_body(self):
        self.create()
        self.source.messages = [self.event()]
        self.poll()
        before = self.row()["cursor"]
        original_session = self.source.session
        def malformed_session(*a, **kw):
            session = original_session(*a, **kw)
            def bad_event(message_id):
                session.step("event:" + message_id)
                return {"messages": [self.event(2)], "next_cursor": "opaque-bad", "generation": GENERATION}
            session.event = bad_event
            return session
        with patch.object(transport, "ReadSession", side_effect=malformed_session):
            item = self.box.pending("planner")[0]
            with self.assertRaisesRegex(inbox.PrivateInboxError, "invalid_response"):
                self.box.read_current("planner", item["notification_id"], key_path=self.root / "key", disclose_private_body=True)
        self.assertEqual(self.row()["cursor"], before)
        self.assertEqual(self.box.status()["notifications"], 1)

    def test_removal_between_room_check_and_body_read_freezes_scope(self):
        self.create()
        self.source.messages = [self.event()]
        self.poll()
        item = self.box.pending("planner")[0]
        def deny_at_event(action):
            if action.startswith("event:"):
                self.source.allowed = False
        self.source.hook = deny_at_event
        with self.assertRaisesRegex(inbox.PrivateInboxError, "scope_unavailable"):
            self.box.read_current("planner", item["notification_id"], key_path=self.root / "key", disclose_private_body=True)
        self.assertEqual(self.box.status()["phase"], "unavailable")
        self.assertEqual(self.box.pending("planner")[0]["state"], "unavailable")

    def test_disk_failure_prevents_body_return_and_rolls_back_observation(self):
        self.create()
        self.source.messages = [self.event()]
        self.poll()
        item = self.box.pending("planner")[0]
        before = self.row("messages")
        with patch.object(self.box, "capacity", side_effect=sqlite3.OperationalError(CANARY)):
            with self.assertRaisesRegex(inbox.PrivateInboxError, "^storage_error$"):
                self.box.read_current("planner", item["notification_id"], key_path=self.root / "key", disclose_private_body=True)
        self.assertEqual(self.row("messages"), before)
        self.assertEqual(self.box.status()["notifications"], 1)

    def test_whole_page_validation_and_admission_are_atomic(self):
        self.create()
        first, second = self.event(), self.event(2)
        second["signature"] = "not-a-signature"
        self.source.messages = [first, second]
        self.assertEqual(self.poll()["last_error"], "invalid_event_signature")
        self.assertEqual(self.box.pending("planner"), [])
        self.assertEqual(self.row()["cursor"], "")
        self.source.messages = [first, self.event(2)]
        with patch.object(inbox, "MAX_EVENTS", 1):
            self.assertEqual(self.poll()["last_error"], "event_capacity")
        self.assertEqual(self.box.pending("planner"), [])
        self.assertEqual(self.row()["cursor"], "")
        self.assertEqual(self.box.status()["notifications"], 0)

    def test_sweep_includes_acknowledged_ids_and_new_pages_do_not_starve(self):
        self.create()
        self.source.messages = [self.event(i) for i in range(1, 4)]
        self.poll()
        for item in self.box.pending("planner"):
            self.box.ack("planner", item["notification_id"], item["snapshot_digest"])
        self.source.calls.clear()
        for i in range(4, 7):
            self.source.messages.append(self.event(i))
            self.poll(max_requests=5)
        self.assertEqual([c for c in self.source.calls if c.startswith("event:")],
                         ["event:event-1", "event:event-2", "event:event-3"])

    def test_storage_permissions_sidecars_lock_and_initial_sync_failure(self):
        with patch.object(inbox.os, "fsync", side_effect=OSError("private-canary")):
            with self.assertRaisesRegex(inbox.PrivateInboxError, "storage_error"):
                self.box.create(consent_private_metadata=True)
            with self.assertRaisesRegex(inbox.PrivateInboxError, "storage_error"):
                self.poll()
        self.create()
        with self.box.database(write=True) as db:
            self.assertEqual(db.execute("PRAGMA synchronous").fetchone()[0], 3)
            with self.assertRaisesRegex(inbox.PrivateInboxError, "inbox_busy"):
                self.box.status()
        sidecar = Path(str(self.path) + "-journal")
        sidecar.write_bytes(b"synthetic-not-a-journal")
        sidecar.chmod(0o644)
        with self.assertRaisesRegex(inbox.PrivateInboxError, "private_regular_file_required"):
            self.box.status()

    def test_offline_hot_journal_recovers_without_network_or_body(self):
        self.create()
        self.source.messages = [self.event()]
        self.poll()
        old = self.row("messages")["metadata"]
        code = "import sqlite3,sys,os; d=sqlite3.connect(sys.argv[1]); d.execute('PRAGMA cache_size=1'); d.execute('BEGIN IMMEDIATE'); d.execute('UPDATE events SET metadata=zeroblob(100000)'); os._exit(79)"
        result = subprocess.run([sys.executable, "-B", "-c", code, str(self.path)])
        self.assertEqual(result.returncode, 79)
        calls = len(self.source.calls)
        self.assertEqual(self.box.status()["phase"], "ready")
        self.assertEqual(self.row("messages")["metadata"], old)
        self.assertEqual(len(self.source.calls), calls)


if __name__ == "__main__":
    unittest.main()
