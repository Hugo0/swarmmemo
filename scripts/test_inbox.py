# SPDX-License-Identifier: Apache-2.0
"""Public inbox fixtures plus optional disposable Go integration."""
from copy import deepcopy
from contextlib import closing
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import socket
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit

CLIENT_DIR = Path(__file__).resolve().parents[1] / "clients/python"
sys.path.insert(0, str(CLIENT_DIR))
import swarmmemo as memo
import swarmmemo_inbox as module

GEN = "b" * 32
RECIPIENT = "a" * 64


def binding(origin="https://example.org", mode="history"):
    return {"version": 1, "origin": origin, "service_id": "swarmmemo.com", "recipient": RECIPIENT,
            "room": "", "visibility": "public", "reader_public_key": "", "start_mode": mode}


def event(identifier="event1", text="public inbox private-local canary", **fields):
    value = {"id": identifier, "sequence": 1, "room": "lobby", "page": "main", "text": text, "kind": "note",
             "author": "anonymous", "created_at": 1788566400, "sha256": module.sha(text.encode()), "to": RECIPIENT,
             "hidden": False, "type": "message", "visibility": "public", "archive_eligible": True}
    value.update(fields)
    return value


def tombstone(record):
    return {**record, "hidden": True, "type": "tombstone", "text": "", "signature": "", "signed_payload": "", "attachments": []}


class Feed:
    def __init__(self, records=None):
        self.records = deepcopy(records or [])
        self.changes = []
        self.generation = GEN
        self.calls = []
        self.hook = None
        self.page_size = 100

    def fetch(self, path, deadline):
        self.calls.append(path)
        parsed = urlsplit(path); query = parse_qs(parsed.query)
        if parsed.path == "/api/changes":
            after = int(query["after"][0])
            if query.get("generation", [self.generation])[0] != self.generation:
                return 409, {"error": {"code": "cursor_reset"}}
            changes = [] if after == -1 else deepcopy(self.changes[after:after+self.page_size])
            next_after = len(self.changes) if after == -1 else after + len(changes)
            return 200, {"ok": True, "messages": changes, "after": next_after, "generation": self.generation, "service_id": "swarmmemo.com"}
        if parsed.path == "/api/messages":
            assert query["to"] == [RECIPIENT]
            cursor = query.get("cursor", [""])[0]
            start = int(cursor.split(":")[1]) if cursor.startswith("opaque:") else 0
            records = deepcopy(self.records[start:start+self.page_size])
            result = {"ok": True, "messages": records, "next_cursor": "opaque:" + str(start + len(records)), "generation": self.generation}
            if self.hook: self.hook(self)
            return 200, result
        if parsed.path.startswith("/e/"):
            identifier = parsed.path[3:]
            for record in self.records:
                if record["id"] == identifier:
                    return 200, {"ok": True, "messages": [deepcopy(record)], "next_cursor": "one", "generation": self.generation}
            return 404, {"error": {"code": "not_found"}}
        raise AssertionError("unexpected request " + path)


class InboxTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.path = Path(self.temp.name) / "inbox.sqlite"
        self.inbox = module.Inbox(self.path, binding())
        self.inbox.create(); self.inbox.add_consumer("planner")
        self.feed = Feed([event()])

    def tearDown(self): self.temp.cleanup()

    def poll(self, **kwargs):
        with patch.object(self.inbox, "_fetch", side_effect=self.feed.fetch): return self.inbox.poll(**kwargs)

    def resync(self, **kwargs):
        with patch.object(self.inbox, "_fetch", side_effect=self.feed.fetch): return self.inbox.resync(**kwargs)

    def checkpoints(self):
        with closing(sqlite3.connect(self.path)) as db, db:
            db.row_factory = sqlite3.Row
            return dict(db.execute("SELECT * FROM checkpoint").fetchone())

    def test_public_binding_rejects_private_key_and_drift_before_network(self):
        for changes in ({"visibility": "private"}, {"reader_public_key": "key"}, {"recipient": ""}, {"origin": "https://example.org/path"}):
            with self.subTest(changes=changes), self.assertRaises(module.InboxError): module.Inbox(self.path, {**binding(), **changes})
        client = memo.Client("https://example.org", memo.crypto()[0].generate())
        with patch.object(self.inbox, "_fetch", side_effect=AssertionError("no request")), self.assertRaises(module.InboxError): self.inbox.poll(client)
        wrong = module.Inbox(self.path, {**binding(), "room": "other"})
        with self.assertRaises(module.InboxError): wrong.status()
        with self.assertRaises(module.InboxError): self.inbox.create(allow_private_storage=True)

    def test_bootstrap_before_events_stage_until_empty_correction_pass(self):
        result = self.poll(max_requests=2)
        self.assertEqual(result["phase"], "corrections")
        self.assertEqual(self.inbox.pending("planner"), [])
        self.assertTrue(self.feed.calls[0].startswith("/api/changes?after=-1"))
        self.assertTrue(self.feed.calls[1].startswith("/api/messages?"))
        self.assertEqual(self.poll()["phase"], "ready")
        pending = self.inbox.pending("planner")
        self.assertEqual(len(pending), 1)
        record = self.inbox.read("planner", pending[0]["notification_id"], sensitive=True)
        self.assertEqual(record["snapshot"]["text"], event()["text"])
        self.assertTrue(record["untrusted_content"])
        self.assertIn("generation=" + GEN, self.feed.calls[2])

    def test_empty_origin_delimiters_and_unsafe_sidecars_rejected(self):
        for origin in ("https://@example.org", "https://example.org?", "https://example.org#", "https://:@example.org"):
            with self.subTest(origin=origin), self.assertRaises(module.InboxError):
                module.Inbox(self.path, {**binding(), "origin": origin})
        for suffix in ("-journal", "-wal", "-shm"):
            sidecar = Path(str(self.path) + suffix)
            sidecar.write_bytes(b"")
            sidecar.chmod(0o644)
            with self.assertRaises(module.InboxError): self.inbox.status()
            self.assertTrue(sidecar.exists())
            sidecar.unlink()
            sidecar.symlink_to(self.path)
            with self.assertRaises(OSError): self.inbox.status()
            sidecar.unlink()

    def test_explicit_signed_handle_cannot_be_forged(self):
        key = memo.crypto()[0].generate()
        client = memo.Client("https://example.org", key)
        command = client.prepare(operation="post", text="signed handle", to=RECIPIENT, handle="original")
        record = event(text=command["text"], author=module.sha(memo.unb64(command["public_key"])),
                       public_key=command["public_key"], signature=command["signature"],
                       signed_payload=memo.canonical(command).decode(), handle="original")
        module.validate_event(record, binding())
        for handle in ("forged", ""):
            with self.subTest(handle=handle), self.assertRaises(module.InboxError):
                module.validate_event({**record, "handle": handle}, binding())
        command = client.prepare(operation="post", text="signed handle", to=RECIPIENT)
        record.update(signature=command["signature"], signed_payload=memo.canonical(command).decode(), handle="server-derived")
        module.validate_event(record, binding())

    def test_local_consumers_acknowledge_independently_and_redact(self):
        self.poll(); self.inbox.add_consumer("worker")
        first = self.inbox.pending("planner")[0]
        with patch.object(self.inbox, "_fetch", side_effect=AssertionError("local operations cannot fetch")):
            self.assertNotIn(event()["text"], json.dumps(self.inbox.status()))
            self.assertNotIn(RECIPIENT, json.dumps(self.inbox.status()))
            self.assertNotIn("snapshot", self.inbox.read("planner", first["notification_id"]))
            with self.assertRaises(module.InboxError): self.inbox.pending("not-created")
            with self.assertRaises(module.InboxError): self.inbox.ack("planner", first["notification_id"], "wrong")
            self.inbox.ack("planner", first["notification_id"], first["snapshot_digest"])
            self.assertTrue(self.inbox.ack("planner", first["notification_id"], first["snapshot_digest"])["duplicate"])
        self.assertEqual(self.inbox.pending("planner"), [])
        self.assertEqual(len(self.inbox.pending("worker")), 1)

    def test_concurrent_tombstone_never_exposes_staged_body_and_latches(self):
        original = deepcopy(self.feed.records[0])
        def hide(feed):
            feed.changes.append(tombstone(original)); feed.records[0] = tombstone(original); feed.hook = None
        self.feed.hook = hide
        self.poll()
        notice = self.inbox.pending("planner")[0]
        self.assertEqual(notice["kind"], "removal")
        self.assertIsNone(self.inbox.read("planner", notice["notification_id"], True)["snapshot"])
        self.feed.records[0] = original; self.feed.changes.append(original)
        self.poll(); self.resync()
        self.assertEqual(self.inbox.pending("planner"), [notice])
        with closing(sqlite3.connect(self.path)) as db, db:
            self.assertEqual(db.execute("SELECT snapshot,latched FROM events").fetchone(), (None, 1))
            self.assertEqual(db.execute("SELECT count(*) FROM notifications").fetchone()[0], 1)

    def test_removal_after_ack_and_obsolete_work_ack(self):
        self.poll(); self.inbox.add_consumer("worker")
        old = self.inbox.pending("planner")[0]
        self.inbox.ack("planner", old["notification_id"], old["snapshot_digest"])
        self.feed.changes.append(tombstone(self.feed.records[0])); self.feed.records[0] = self.feed.changes[0]
        self.poll()
        with self.assertRaises(module.InboxError): self.inbox.ack("worker", old["notification_id"], old["snapshot_digest"])
        with self.assertRaises(module.InboxError): self.inbox.read("planner", old["notification_id"], True)
        self.assertEqual(self.inbox.pending("planner")[0]["kind"], "removal")

    def test_unknown_corrections_not_retained_and_short_pages_not_completion(self):
        self.feed.records.append(event("event2", "second")); self.feed.page_size = 1
        self.poll()
        self.assertFalse(self.inbox.status()["event_page_empty"])
        self.feed.changes.append(event("unrelated", "unrelated secret text", to=""))
        self.poll(); self.poll()
        self.assertTrue(self.inbox.status()["event_page_empty"])
        with closing(sqlite3.connect(self.path)) as db, db:
            self.assertEqual(db.execute("SELECT count(*) FROM events").fetchone()[0], 2)
            self.assertEqual(db.execute("SELECT count(*) FROM events WHERE id='unrelated'").fetchone()[0], 0)

    def test_signature_exact_bytes_alias_recipient_and_invalid_event_rollback(self):
        key = memo.crypto()[0].generate()
        command = memo.sign({"operation": "post", "text": "signed", "to": "c"*64}, key)
        signed = event(text="signed", to="c"*64, author=module.sha(memo.public_bytes(key)), public_key=command["public_key"],
                       signature=command["signature"], signed_payload=memo.canonical(command).decode())
        self.feed.records = [signed]
        self.poll()
        row = self.inbox.pending("planner")[0]
        self.assertEqual(self.inbox.read("planner", row["notification_id"], True)["snapshot"]["signed_payload"], signed["signed_payload"])
        self.feed.records.append(event("bad", "wrong hash", sha256="f"*64))
        before = self.checkpoints()["cursor"]
        result = self.poll()
        self.assertEqual(result["last_error"], "event_hash_or_state_mismatch")
        self.assertEqual(self.checkpoints()["cursor"], before)
        with self.assertRaises(module.InboxError): self.inbox.read("planner", row["notification_id"], True)

    def test_generation_reset_requires_explicit_bounded_resync_without_duplicate_work(self):
        self.poll(); notice = self.inbox.pending("planner")[0]
        self.inbox.ack("planner", notice["notification_id"], notice["snapshot_digest"])
        self.feed.generation = "d"*32
        self.assertEqual(self.poll()["phase"], "resync_required")
        with self.assertRaises(module.InboxError): self.poll()
        self.assertEqual(self.resync(max_requests=2)["resync_active"], True)
        self.assertEqual(self.inbox.pending("planner"), [])
        with self.assertRaises(module.InboxError): self.poll()
        self.assertEqual(self.resync()["phase"], "ready")
        self.assertEqual(self.inbox.pending("planner"), [])
        self.assertEqual(self.inbox.status()["notifications"], 1)

    def test_reset_during_resync_requires_new_explicit_restart(self):
        self.poll()
        self.resync(max_requests=1)
        self.feed.generation = "e"*32
        self.assertEqual(self.resync()["phase"], "resync_required")
        self.assertFalse(self.inbox.status()["resync_active"])
        self.assertEqual(self.resync()["phase"], "ready")

    def test_resync_revalidates_missing_retained_ids_and_purges_404(self):
        self.poll(); self.feed.records = []
        self.assertEqual(self.resync()["phase"], "ready")
        self.assertTrue(any(path.startswith("/e/event1") for path in self.feed.calls))
        self.assertEqual(self.inbox.pending("planner")[0]["state"], "unavailable")
        with closing(sqlite3.connect(self.path)) as db, db: self.assertEqual(db.execute("SELECT snapshot FROM events").fetchone()[0], None)

    def test_missing_generation_unknown_fields_and_private_response_fail_closed(self):
        for mutation in ("missing_generation", "unknown", "private"):
            fresh = module.Inbox(Path(self.temp.name) / (mutation + ".sqlite"), binding()); fresh.create()
            def fetch(path, deadline):
                status, result = self.feed.fetch(path, deadline)
                if path.startswith("/api/messages"):
                    if mutation == "missing_generation": del result["generation"]
                    elif mutation == "unknown": result["messages"][0]["future_payload"] = "do not persist"
                    else: result["messages"][0]["visibility"] = "private"
                return status, result
            with patch.object(fresh, "_fetch", side_effect=fetch): result = fresh.poll()
            self.assertNotEqual(result["phase"], "ready")
            self.assertEqual(result["messages"], {})

    def test_message_pages_accept_only_has_more_data(self):
        for data, accepted in (({"has_more": False}, True), ({"has_more": "no"}, False), ({"has_more": False, "extra": 1}, False), ([], False)):
            fresh = module.Inbox(Path(self.temp.name) / ("data" + str(len(self.feed.calls)) + ".sqlite"), binding()); fresh.create()
            def fetch(path, deadline):
                status, result = self.feed.fetch(path, deadline)
                if path.startswith("/api/messages"): result["data"] = data
                return status, result
            with self.subTest(data=data), patch.object(fresh, "_fetch", side_effect=fetch):
                fresh.poll()
                self.assertEqual(fresh.poll()["phase"] == "ready", accepted)

    def test_service_derived_removal_and_curator_fields(self):
        for remover in ("operator", "room"):
            module.validate_event(tombstone(event(hidden_by=remover)), binding())
        module.validate_event(event(curated=True), binding())
        for record in (tombstone(event(hidden_by="author")), event(hidden_by="operator"), event(curated=False), tombstone(event(curated=True))):
            with self.subTest(record=record), self.assertRaises(module.InboxError): module.validate_event(record, binding())

    def test_via_is_a_channel_token(self):
        for via in ("dns", "x-text", "ui"):
            module.validate_event(event(via=via), binding())
            module.validate_event(tombstone(event(hidden_by="operator", via=via)), binding())
        for via in ("", "DNS", "-dns", "a" * 17, 1, None):
            with self.subTest(via=via), self.assertRaises(module.InboxError): module.validate_event(event(via=via), binding())

    def test_bridge_provenance_is_service_shaped_and_unsigned(self):
        carried = {"mode": "reissued", "origin_service": "nostr", "origin_id": "ab" * 32, "origin_author": "npub1xyz", "origin_ref": "nostr:nevent1xyz"}
        module.validate_event(event(forwarded=carried), binding())
        for bad in (dict(carried, mode="verbatim"), dict(carried, origin_author="has space"), {"mode": "reissued"}, "nostr"):
            with self.subTest(bad=bad), self.assertRaises(module.InboxError): module.validate_event(event(forwarded=bad), binding())

    def test_capacity_does_not_advance_event_cursor_but_applies_removal(self):
        self.poll(); old_cursor = self.checkpoints()["cursor"]
        original = self.feed.records[0]
        self.feed.records.append(event("new", "cannot fit")); self.feed.changes.append(tombstone(original))
        with patch.object(module, "BODY_BUDGET", 1): result = self.poll()
        self.assertEqual(self.checkpoints()["cursor"], old_cursor)
        self.assertEqual(result["messages"].get("tombstoned"), 1)
        with closing(sqlite3.connect(self.path)) as db, db: self.assertIsNone(db.execute("SELECT snapshot FROM events WHERE id='event1'").fetchone()[0])

    def test_offline_hot_journal_atomic_init_and_extra(self):
        self.poll()
        with self.inbox.database(write=True) as db: self.assertEqual(db.execute("PRAGMA synchronous").fetchone()[0], 3)
        code = "import os,sqlite3,sys; db=sqlite3.connect(sys.argv[1]); db.execute('PRAGMA cache_size=1'); db.execute('BEGIN IMMEDIATE'); db.execute('UPDATE events SET snapshot=zeroblob(100000)'); os._exit(79)"
        child = subprocess.run([sys.executable, "-c", code, str(self.path)], timeout=10)
        self.assertEqual(child.returncode, 79)
        with patch.object(self.inbox, "_fetch", side_effect=AssertionError("offline")):
            self.assertEqual(self.inbox.status()["phase"], "ready")
            notice = self.inbox.pending("planner")[0]
            self.assertEqual(self.inbox.read("planner", notice["notification_id"], True)["snapshot"]["text"], event()["text"])
        bad = module.Inbox(Path(self.temp.name) / "bad.sqlite", binding())
        with patch.object(module, "SCHEMA", module.SCHEMA + "INVALID SQL;"):
            with self.assertRaises(sqlite3.OperationalError): bad.create()
        bad.create()

    def test_client_callbacks_cannot_override_fixed_unsigned_transport(self):
        client = memo.Client("https://example.org")
        client._request = lambda *args: self.fail("custom client callback invoked")
        with patch.object(self.inbox, "_fetch", side_effect=self.feed.fetch): self.inbox.poll(client)
        self.assertEqual(self.inbox.status()["phase"], "ready")

    def test_attachment_correction_no_asset_fetch_and_obsolete_digest_cannot_reappear(self):
        attachment = {"id": "blob1", "room": "lobby", "filename": "untrusted.txt", "media_type": "text/plain", "sha256": "1"*64,
                      "size": 10, "created_at": 1, "expires_at": 100, "deleted": False, "expired": False}
        self.feed.records[0]["attachments"] = [attachment]
        self.poll(); first = self.inbox.pending("planner")[0]
        changed = deepcopy(self.feed.records[0]); changed["attachments"][0]["deleted"] = True
        self.feed.changes.append(changed); self.poll()
        correction = self.inbox.pending("planner")[0]
        self.assertEqual(correction["kind"], "correction")
        self.assertTrue(self.inbox.read("planner", correction["notification_id"], True)["snapshot"]["attachments"][0]["deleted"])
        self.feed.changes.append(deepcopy(self.feed.records[0])); self.poll()
        self.assertEqual(len(self.inbox.pending("planner")), 1)
        with self.assertRaises(module.InboxError): self.inbox.ack("planner", first["notification_id"], first["snapshot_digest"])
        self.assertTrue(all(path.startswith(("/api/messages?", "/api/changes?")) for path in self.feed.calls))

    def test_capacity_during_resync_never_promotes_incomplete_history(self):
        self.poll()
        self.feed.records.append(event("cannot-add", "capacity"))
        original = deepcopy(self.feed.records[0])
        def hide(feed): feed.changes.append(tombstone(original)); feed.hook = None
        self.feed.hook = hide
        with patch.object(module, "MAX_EVENTS", 1): result = self.resync()
        self.assertTrue(result["resync_active"])
        self.assertNotEqual(result["phase"], "ready")
        self.assertEqual(result["messages"].get("tombstoned"), 1)

    def test_process_crash_before_and_after_event_commit_preserves_checkpoints(self):
        code = """
import json,os,sys
sys.path.insert(0,sys.argv[1])
import swarmmemo_inbox as m
path,config,record,when=sys.argv[2:]
inbox=m.Inbox(path,json.loads(config))
record=json.loads(record)
def fetch(path,deadline):
    if path.startswith('/api/messages'):
        return 200,{'ok':True,'messages':[record],'next_cursor':'opaque:1','generation':'b'*32}
    if 'after=-1' not in path and when=='after': os._exit(82)
    return 200,{'ok':True,'messages':[],'after':0,'generation':'b'*32,'service_id':'swarmmemo.com'}
inbox._fetch=fetch
if when=='before':
    original=inbox.receive
    def interrupted(*args): original(*args); os._exit(81)
    inbox.receive=interrupted
inbox.poll()
"""
        for when in ("before", "after"):
            with self.subTest(when=when):
                path = Path(self.temp.name) / (when + ".sqlite")
                inbox = module.Inbox(path, binding()); inbox.create(); inbox.add_consumer("reader")
                child = subprocess.run([sys.executable, "-c", code, str(CLIENT_DIR), str(path), json.dumps(binding()), json.dumps(event()), when], timeout=10)
                self.assertEqual(child.returncode, 81 if when == "before" else 82)
                with closing(sqlite3.connect(path)) as db, db:
                    cursor = db.execute("SELECT cursor FROM checkpoint").fetchone()[0]
                    self.assertEqual(cursor, "start" if when == "before" else "opaque:1")
                self.assertEqual(inbox.pending("reader"), [])
                feed = Feed([event()])
                with patch.object(inbox, "_fetch", side_effect=feed.fetch): inbox.poll(); inbox.poll()
                self.assertEqual(inbox.status()["notifications"], 1)

    def test_resync_wrong_scope_known_event_purges_without_retaining_private_response(self):
        self.poll(); self.feed.records = []
        original_fetch = self.feed.fetch
        def wrong_scope(path, deadline):
            if path.startswith("/e/"):
                return 200, {"ok": True, "messages": [event(visibility="private", text="PRIVATE_SCOPE_CANARY")], "next_cursor": "one", "generation": GEN}
            return original_fetch(path, deadline)
        with patch.object(self.inbox, "_fetch", side_effect=wrong_scope): self.inbox.resync()
        with closing(sqlite3.connect(self.path)) as db, db:
            self.assertEqual(db.execute("SELECT snapshot,state FROM events").fetchone(), (None, "unavailable"))

    def test_repeated_event_cursor_and_limits_fail_without_advancement(self):
        self.poll(); before = self.checkpoints()["cursor"]
        original = self.feed.fetch
        def repeated(path, deadline):
            if path.startswith("/api/messages"):
                return 200, {"ok": True, "messages": [event()], "next_cursor": before, "generation": GEN}
            return original(path, deadline)
        with patch.object(self.inbox, "_fetch", side_effect=repeated): result = self.inbox.poll()
        self.assertEqual(result["last_error"], "event_no_progress")
        self.assertEqual(self.checkpoints()["cursor"], before)
        for kwargs in ({"max_requests": 21}, {"deadline_seconds": 31}):
            with self.assertRaises(module.InboxError): self.inbox.poll(**kwargs)
        with patch.object(module, "MAX_CONSUMERS", 1), self.assertRaises(module.InboxError): self.inbox.add_consumer("extra")

    def test_event_size_corrupt_signature_and_cached_body_integrity(self):
        with self.assertRaises(module.InboxError): module.validate_event(event(text="x" * module.MAX_EVENT), binding(), addressed=True)
        signed = event(public_key=memo.b64(b"a"*32), signature=memo.b64(b"a"*64), signed_payload='{}')
        with self.assertRaises(module.InboxError): module.validate_event(signed, binding(), addressed=True)
        self.poll(); notice = self.inbox.pending("planner")[0]
        with closing(sqlite3.connect(self.path)) as db, db: db.execute("UPDATE events SET snapshot=?", (module.encode(event(text="tampered cache")),))
        with self.assertRaisesRegex(module.InboxError, "cache_integrity_error"): self.inbox.read("planner", notice["notification_id"], True)


class TransportTests(unittest.TestCase):
    def test_killable_worker_deadline_including_dns_stall(self):
        inbox = module.Inbox(Path("unused"), binding("http://127.0.0.1:9"))
        real_popen = subprocess.Popen
        children = []
        code = "import os,runpy,socket,sys,time; sys.path.insert(0,sys.argv[1]); socket.getaddrinfo=lambda *a,**k: time.sleep(10); sys.argv=[sys.argv[2],'_fetch',str(os.getppid())]; runpy.run_path(sys.argv[0],run_name='__main__')"
        def slow_dns(args, **kwargs):
            process = real_popen([sys.executable, "-I", "-B", "-c", code, str(CLIENT_DIR), str(CLIENT_DIR / "swarmmemo_inbox.py")], **kwargs)
            children.append(process); return process
        start = time.monotonic()
        with patch.object(module.subprocess, "Popen", side_effect=slow_dns), self.assertRaisesRegex(module.InboxError, "deadline_exceeded"):
            inbox._fetch("/api/changes?after=-1", time.monotonic()+0.3)
        self.assertLess(time.monotonic()-start, 1.5)
        self.assertTrue(all(child.poll() is not None for child in children))

    def test_trickle_truncation_duplicate_json_redirect_and_response_cap(self):
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_): pass
            def do_GET(self):
                if self.path == "/redirect":
                    self.send_response(302); self.send_header("Location", "/ok"); self.end_headers(); return
                self.send_response(200); self.send_header("Content-Type", "application/json")
                if self.path == "/truncated": self.send_header("Content-Length", "100")
                if self.path == "/oversized": self.send_header("Content-Length", str(module.MAX_RESPONSE+1))
                self.end_headers()
                if self.path == "/trickle":
                    try:
                        for _ in range(30): self.wfile.write(b" "); self.wfile.flush(); time.sleep(.1)
                    except OSError: pass
                elif self.path == "/duplicate": self.wfile.write(b'{"ok":true,"ok":false}')
                else: self.wfile.write(b'{}'); self.close_connection = True
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        server.daemon_threads = True
        thread = threading.Thread(target=server.serve_forever, daemon=True); thread.start()
        inbox = module.Inbox(Path("unused"), binding("http://127.0.0.1:" + str(server.server_port)))
        try:
            for path in ("/truncated", "/duplicate", "/redirect", "/oversized", "/trickle"):
                with self.subTest(path=path), self.assertRaises(module.InboxError): inbox._fetch(path, time.monotonic()+.5)
        finally: server.shutdown(); server.server_close(); thread.join()


@unittest.skipUnless(os.environ.get("SWARMMEMO_TEST_BINARY"), "set generation-capable SWARMMEMO_TEST_BINARY")
class GoInboxTests(unittest.TestCase):
    def test_real_public_signed_private_exclusion_moderation_and_restart(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with socket.socket() as sock: sock.bind(("127.0.0.1", 0)); port = sock.getsockname()[1]
            origin = "http://127.0.0.1:" + str(port)
            env = {**os.environ, "DATA_DIR": str(root / "server"), "LISTEN_ADDR": "127.0.0.1:" + str(port), "ALLOW_INSECURE_LOCAL": "true"}
            process = subprocess.Popen([os.environ["SWARMMEMO_TEST_BINARY"], "serve"], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                for _ in range(100):
                    try:
                        connection = http.client.HTTPConnection("127.0.0.1", port, timeout=1); connection.request("GET", "/health")
                        response = connection.getresponse(); response.read(); connection.close()
                        if response.status == 200: break
                    except OSError: time.sleep(.05)
                else: self.fail("server startup")
                key = memo.crypto()[0].generate(); sender = memo.Client(origin, key)
                recipient = memo.Client(origin, memo.crypto()[0].generate())
                recipient.command("agent.register")
                target = module.sha(memo.public_bytes(recipient.key))
                public = sender.post("lobby", "main", "actual signed addressed", to=target)["receipt"]["id"]
                sender.command("room.create", room="private-inbox", visibility="private")
                sender.post("private-inbox", "main", "PRIVATE_NEVER_COLLECTED", to=target)
                config = {**binding(origin), "recipient": target}
                inbox = module.Inbox(root / "inbox.sqlite", config); inbox.create(); inbox.add_consumer("reader")
                self.assertEqual(inbox.poll()["phase"], "ready")
                first = inbox.pending("reader")[0]
                self.assertEqual(inbox.read("reader", first["notification_id"], True)["snapshot"]["text"], "actual signed addressed")
                inbox.ack("reader", first["notification_id"], first["snapshot_digest"])
                moderated = subprocess.run([os.environ["SWARMMEMO_TEST_BINARY"], "moderate", public, "hide", "fixture moderation"], env=env, capture_output=True, timeout=10)
                self.assertEqual(moderated.returncode, 0, moderated.stderr.decode())
                restarted = module.Inbox(root / "inbox.sqlite", config)
                self.assertEqual(restarted.poll()["phase"], "ready")
                notice = restarted.pending("reader")[0]
                self.assertEqual(notice["kind"], "removal")
                self.assertIsNone(restarted.read("reader", notice["notification_id"], True)["snapshot"])
                with closing(sqlite3.connect(root / "inbox.sqlite")) as db, db:
                    self.assertEqual(db.execute("SELECT count(*) FROM events").fetchone()[0], 1)
                    self.assertEqual(db.execute("SELECT count(*) FROM events WHERE snapshot IS NOT NULL").fetchone()[0], 0)
            finally:
                process.terminate()
                try: process.wait(timeout=5)
                except subprocess.TimeoutExpired: process.kill(); process.wait()


if __name__ == "__main__": unittest.main()
