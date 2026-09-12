"""Outbox fixtures; optional true Go-server/process crash test via SWARMMEMO_TEST_BINARY."""
import contextlib
import hashlib
import http.client
import io
import json
import os
from pathlib import Path
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import Mock, patch

CLIENT_DIR = Path(__file__).resolve().parents[1] / "clients/python"
sys.path.insert(0, str(CLIENT_DIR))
import swarmmemo as memo
import swarmmemo_outbox as outbox


def post(text="private local outbox canary"):
    return {"operation": "post", "room": "lobby", "page": "main", "text": text}


def acknowledgement(command):
    return {"ok": True, "receipt": {"id": "event-fixture", "sha256": outbox.sha(command["text"].encode()),
                                    "accepted_at": 1788566400, "duplicate": False}}


class OutboxTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.path = Path(self.temp.name) / "private" / "outbox.sqlite"
        self.path.parent.mkdir(mode=0o700)
        self.key = memo.crypto()[0].generate()
        self.public = memo.b64(memo.public_bytes(self.key))
        self.queue = outbox.Outbox(self.path, "https://example.org", self.public)
        self.client = memo.Client("https://example.org", self.key, timeout=10)
        self.client.send = Mock(side_effect=acknowledgement)

    def tearDown(self): self.temp.cleanup()

    def test_default_status_no_network_no_files_and_no_crypto_for_anonymous(self):
        with patch.object(memo, "crypto", side_effect=AssertionError("anonymous must not load crypto")):
            queue = outbox.Outbox(self.path, "https://example.org", "")
            self.assertEqual(queue.status()["items"], [])
            self.assertFalse(self.path.exists())
            queue.enqueue("anon", post())
            client = memo.Client("https://example.org", timeout=10)
            client.send = Mock(side_effect=acknowledgement)
            self.assertEqual(queue.flush(client)[0]["state"], "acknowledged")
            self.assertNotIn("signature", client.send.call_args.args[0])
            with self.assertRaisesRegex(outbox.OutboxError, "anonymous_post_only"):
                queue.enqueue("other", {"operation": "room.create", "room": "private"})

    def test_enqueue_is_unsigned_immutable_and_status_redacts(self):
        with patch.object(memo.Client, "prepare", side_effect=AssertionError("enqueue cannot sign")):
            first = self.queue.enqueue("caller-1", post())
            again = self.queue.enqueue("caller-1", dict(reversed(list(post().items()))))
        self.assertEqual(first, again)
        self.assertIsNone(first["envelope_sha256"])
        with self.assertRaisesRegex(outbox.OutboxError, "intent_id_conflict"):
            self.queue.enqueue("caller-1", post("different"))
        self.assertNotIn(post()["text"], json.dumps(self.queue.status()))
        self.assertNotIn("intent", self.queue.inspect("caller-1"))
        self.assertEqual(self.queue.inspect("caller-1", sensitive=True)["intent"], post())

    def test_signed_envelope_is_committed_before_network_and_wire_identical(self):
        self.queue.enqueue("durable-before-send", post())
        def send(command):
            with contextlib.closing(sqlite3.connect(self.path)) as db, db:
                envelope, state, attempts = db.execute("SELECT envelope,state,attempts FROM queue").fetchone()
            self.assertEqual(envelope, json.dumps(command, ensure_ascii=False).encode())
            self.assertEqual(state, "unresolved"); self.assertEqual(attempts, 1)
            self.assertEqual(command["public_key"], self.public)
            return acknowledgement(command)
        self.client.send.side_effect = send
        self.assertEqual(self.queue.flush(self.client)[0]["state"], "acknowledged")
        self.assertEqual(self.queue.flush(self.client), [])
        self.assertEqual(self.client.send.call_count, 1)

    def test_lost_accepted_response_restart_reuses_exact_old_signed_envelope(self):
        self.queue.enqueue("accepted-but-lost", post())
        accepted = []
        def lost(command):
            accepted.append(outbox.encoded(command))
            raise TimeoutError("SECRET_NETWORK_ERROR")
        self.client.send.side_effect = lost
        result = self.queue.flush(self.client)
        self.assertEqual(result[0]["state"], "unresolved")
        self.assertNotIn("SECRET", json.dumps(result))
        restarted = outbox.Outbox(self.path, "https://example.org", self.public)
        def duplicate(command):
            self.assertEqual(outbox.encoded(command), accepted[0])
            return {**acknowledgement(command), "receipt": {**acknowledgement(command)["receipt"], "duplicate": True}}
        self.client.send.side_effect = duplicate
        with patch.object(self.client, "prepare", side_effect=AssertionError("must never re-sign")), patch.object(outbox.time, "time", return_value=int(time.time())+3600):
            self.assertEqual(restarted.flush(self.client)[0]["state"], "acknowledged")
        self.assertTrue(restarted.inspect("accepted-but-lost", sensitive=True)["response"]["receipt"]["duplicate"])

    def test_unaccepted_expired_envelope_is_blocked_never_replaced(self):
        self.queue.enqueue("never-accepted", post())
        self.client.send.side_effect = ConnectionError("offline")
        self.queue.flush(self.client)
        envelope = self.queue.inspect("never-accepted", sensitive=True)["envelope"]
        self.client.send.side_effect = memo.APIError(401, "stale_signature", "server details")
        with patch.object(self.client, "prepare", side_effect=AssertionError("must never re-sign")):
            self.assertEqual(self.queue.flush(self.client)[0]["state"], "blocked")
            calls = self.client.send.call_count
            self.queue.flush(self.client)
            self.assertEqual(calls, self.client.send.call_count)
            self.queue.flush(self.client, retry_blocked=True)
        self.assertEqual(self.queue.inspect("never-accepted", sensitive=True)["envelope"], envelope)
        self.assertEqual(self.queue.inspect("never-accepted")["last_error"], "stale_signature")

    def test_operation_specific_data_success_and_false_success_rejected(self):
        self.queue.enqueue("register", {"operation": "agent.register", "handle": "Fixture"})
        self.client.send.side_effect = lambda cmd: {"ok": True, "data": {"agent_id": outbox.sha(memo.unb64(self.public)), "handle": "fixture"}}
        self.assertEqual(self.queue.flush(self.client)[0]["state"], "acknowledged")
        self.queue.enqueue("false-post", post())
        self.client.send.side_effect = lambda cmd: {"ok": True, "data": {"private_echo": post()["text"]}}
        self.assertEqual(self.queue.flush(self.client)[0]["state"], "unresolved")
        self.assertNotIn(post()["text"], json.dumps(self.queue.status()))

    def test_blob_intent_and_acknowledgement_integrity(self):
        content = b"blob-fixture"
        self.queue.enqueue("blob", {"operation": "blob.put", "room": "lobby", "data": memo.b64(content), "filename": "a.txt", "media_type": "text/plain"})
        self.client.send.side_effect = lambda cmd: {"ok": True, "data": {"blob": {"id": "blob-id", "room": "lobby", "sha256": outbox.sha(content), "size": len(content)}}}
        self.assertEqual(self.queue.flush(self.client)[0]["state"], "acknowledged")

    def test_rotation_is_barrier_after_success_and_across_restart(self):
        successor = memo.crypto()[0].generate()
        successor_public = memo.b64(memo.public_bytes(successor))
        self.queue.enqueue("rotate", {"operation": "agent.rotate", "target": successor_public})
        self.queue.enqueue("old-key-after", post())
        self.client.send.side_effect = lambda cmd: {"ok": True, "data": {"agent_id": outbox.sha(memo.unb64(successor_public)), "predecessor": outbox.sha(memo.unb64(self.public))}}
        result = self.queue.flush(self.client, rotation_key=successor)
        self.assertEqual(len(result), 1); self.assertEqual(result[0]["state"], "acknowledged")
        self.assertEqual(self.queue.inspect("old-key-after")["state"], "queued")
        with self.assertRaisesRegex(outbox.OutboxError, "outbox_rotation_completed"):
            self.queue.flush(self.client)
        self.assertEqual(self.client.send.call_count, 1)

    def test_unresolved_rotation_and_earlier_errors_stop_later_work(self):
        self.queue.enqueue("first", post())
        self.queue.enqueue("second", post("later"))
        self.client.send.side_effect = TimeoutError("lost")
        self.assertEqual(len(self.queue.flush(self.client)), 1)
        self.assertEqual(self.queue.inspect("second")["state"], "queued")

    def test_lost_rotation_response_retries_without_successor_private_key(self):
        successor = memo.crypto()[0].generate()
        target = memo.b64(memo.public_bytes(successor))
        self.queue.enqueue("rotation-lost", {"operation": "agent.rotate", "target": target})
        self.queue.enqueue("after-rotation", post())
        self.client.send.side_effect = TimeoutError("accepted rotation response lost")
        self.assertEqual(self.queue.flush(self.client, rotation_key=successor)[0]["state"], "unresolved")
        original = self.queue.inspect("rotation-lost", sensitive=True)["envelope"]
        self.client.send.side_effect = lambda command: {"ok": True, "data": {"agent_id": outbox.sha(memo.unb64(target)), "predecessor": outbox.sha(memo.unb64(self.public))}}
        with patch.object(self.client, "prepare", side_effect=AssertionError("do not re-sign rotation")):
            self.assertEqual(self.queue.flush(self.client)[0]["state"], "acknowledged")
        self.assertEqual(self.client.send.call_args.args[0], original)
        self.assertEqual(self.queue.inspect("after-rotation")["state"], "queued")

    def test_new_queue_signs_at_flush_not_enqueue_and_reserves_delivery_capacity(self):
        with patch.object(outbox.time, "time", return_value=100): self.queue.enqueue("waited", post())
        with patch.object(outbox.time, "time", return_value=100000): self.queue.flush(self.client)
        self.assertEqual(self.client.send.call_args.args[0]["timestamp"], 100000)
        bounded = outbox.Outbox(self.path.parent / "bounded.sqlite", "https://example.org", self.public, max_bytes=4*1024*1024)
        blob = {"operation": "blob.put", "room": "lobby", "data": memo.b64(b"a" * 1048576)}
        bounded.enqueue("large-1", blob)
        with self.assertRaisesRegex(outbox.OutboxError, "queue_byte_limit"): bounded.enqueue("large-2", blob)

    def test_corrupted_persisted_envelope_never_sends(self):
        self.queue.enqueue("corrupt-wire", post())
        self.client.send.side_effect = TimeoutError("lost")
        self.queue.flush(self.client)
        with contextlib.closing(sqlite3.connect(self.path)) as db, db: db.execute("UPDATE queue SET envelope=envelope||' '")
        self.client.send.reset_mock()
        result = self.queue.flush(self.client)
        self.assertEqual(result[0]["state"], "blocked")
        self.assertEqual(self.client.send.call_count, 0)

    def test_binding_mismatch_external_save_and_symlink_permissions_rejected(self):
        self.queue.enqueue("bound", post())
        for field, value in (("base_url", "https://other.example"), ("service", "other.example"), ("key", memo.crypto()[0].generate()), ("save_request", "secret-copy.json")):
            previous = getattr(self.client, field)
            setattr(self.client, field, value)
            with self.subTest(field=field), self.assertRaises(outbox.OutboxError): self.queue.flush(self.client)
            setattr(self.client, field, previous)
        other = outbox.Outbox(self.path, "https://other.example", self.public)
        with self.assertRaisesRegex(outbox.OutboxError, "outbox_binding_mismatch"): other.status()
        self.assertEqual(self.client.send.call_count, 0)
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)
        self.path.chmod(0o644)
        with self.assertRaises(outbox.OutboxError): self.queue.status()
        self.path.chmod(0o600)
        link = self.path.parent / "link.sqlite"; link.symlink_to(self.path)
        with self.assertRaises(OSError): outbox.Outbox(link, "https://example.org", self.public).status()

    def test_lock_queue_capacity_field_allowlist_and_integrity(self):
        self.queue.enqueue("one", post())
        with self.queue.database(write=True):
            with self.assertRaisesRegex(outbox.OutboxError, "outbox_busy"): self.queue.flush(self.client)
        with patch.object(outbox, "MAX_ROWS", 1):
            with self.assertRaisesRegex(outbox.OutboxError, "queue_item_limit"): self.queue.enqueue("two", post())
        for fields in ({"operation": "messages.list"}, {**post(), "timestamp": 1}, {**post(), "request_id": "override"}, {"operation": "future.publish"}):
            with self.subTest(fields=fields), self.assertRaises(outbox.OutboxError): self.queue.enqueue("invalid", fields)
        with contextlib.closing(sqlite3.connect(self.path)) as db, db: db.execute("UPDATE queue SET intent_sha256='corrupt'")
        self.assertEqual(self.queue.flush(self.client)[0]["last_error"], "outbox_integrity_error")
        self.assertEqual(self.client.send.call_count, 0)

    def test_default_cli_status_and_duplicate_json_are_safe(self):
        with contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(outbox.main(["--db", str(self.path), "--public-key=" + self.public]), 0)
        self.assertEqual(json.loads(output.getvalue())["network_requests"], 0)
        with self.assertRaises(outbox.OutboxError): outbox.strict_json(b'{"operation":"post","operation":"report"}')

    def test_durable_journal_mode_atomic_initialization_and_explicit_parent(self):
        absent = outbox.Outbox(self.path.parent / "absent" / "queue.sqlite", "https://example.org", self.public)
        self.assertEqual(absent.status()["items"], [])
        with self.assertRaisesRegex(outbox.OutboxError, "private_directory_required"): absent.enqueue("no-parent", post())
        self.assertFalse(absent.path.parent.exists())
        with patch.object(outbox, "SCHEMA", outbox.SCHEMA + "\nTHIS IS INVALID SQL;"):
            with self.assertRaises(sqlite3.OperationalError): self.queue.enqueue("interrupted-init", post())
        with contextlib.closing(sqlite3.connect(self.path)) as db, db:
            self.assertEqual(db.execute("PRAGMA user_version").fetchone()[0], 0)
            self.assertEqual(db.execute("SELECT count(*) FROM sqlite_master WHERE type='table'").fetchone()[0], 0)
        self.queue.enqueue("after-init-recovery", post())
        with self.queue.database(write=True) as db:
            self.assertEqual(db.execute("PRAGMA synchronous").fetchone()[0], 3)
            self.assertEqual(db.execute("PRAGMA journal_mode").fetchone()[0], "delete")

    def test_offline_status_recovers_hot_journal_after_process_crash(self):
        intent = post("x" * 100000)
        self.queue.enqueue("hot-journal", intent)
        code = """
import os,sqlite3,sys
db=sqlite3.connect(sys.argv[1])
db.execute('PRAGMA cache_size=1')
db.execute('BEGIN IMMEDIATE')
db.execute('UPDATE queue SET intent=zeroblob(100000)')
os._exit(79)
"""
        child = subprocess.run([sys.executable, "-c", code, str(self.path)], capture_output=True, timeout=10)
        self.assertEqual(child.returncode, 79)
        self.assertTrue(Path(str(self.path) + "-journal").exists())
        with patch.object(memo.Client, "send", side_effect=AssertionError("status must stay offline")):
            self.assertEqual(self.queue.status()["counts"], {"queued": 1})
            self.assertEqual(self.queue.inspect("hot-journal", sensitive=True)["intent"], intent)

    def test_malformed_acknowledgements_fail_closed(self):
        cases = [
            ({"operation": "room.create"}, {"ok": True, "data": {"visibility": "public"}}),
            ({"operation": "room.member.add"}, {"ok": True, "data": {"operation": "room.member.add"}}),
            ({"operation": "credit.transfer", "target": "target", "amount": 1}, {"ok": True, "data": {"recipient": "target", "transferred_bytes": True}}),
            ({"operation": "lease.release", "amount": 1}, {"ok": True, "data": {"released": True, "fence": True}}),
            ({"operation": "blob.delete"}, {"ok": True, "data": {"blob": {"deleted": True}}}),
        ]
        for command, result in cases:
            with self.subTest(command=command), self.assertRaises(outbox.OutboxError): outbox.validate_ack(command, result)


@unittest.skipUnless(os.environ.get("SWARMMEMO_TEST_BINARY"), "set SWARMMEMO_TEST_BINARY for real process recovery")
class OutboxProcessIntegration(unittest.TestCase):
    def test_process_dies_after_server_acceptance_then_recovers_one_post(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with socket.socket() as sock:
                sock.bind(("127.0.0.1", 0)); port = sock.getsockname()[1]
            origin = "http://127.0.0.1:" + str(port)
            process = subprocess.Popen([os.environ["SWARMMEMO_TEST_BINARY"], "serve"],
                env={**os.environ, "DATA_DIR": str(root / "server"), "LISTEN_ADDR": "127.0.0.1:" + str(port), "ALLOW_INSECURE_LOCAL": "true"},
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                for _ in range(100):
                    try:
                        connection = http.client.HTTPConnection("127.0.0.1", port, timeout=1)
                        connection.request("GET", "/health")
                        reply = connection.getresponse(); reply.read(); connection.close()
                        if reply.status == 200: break
                    except OSError: time.sleep(0.05)
                else: self.fail("test server did not start")
                key_path = root / "key.json"
                public = memo.keygen(key_path)["public_key"]
                path = root / "outbox.sqlite"
                queue = outbox.Outbox(path, origin, public)
                queue.enqueue("process-crash", post("process-crash-unique-canary"))
                code = """
import os,sys
from pathlib import Path
import swarmmemo as memo
import swarmmemo_outbox as outbox
path,url,public,key=sys.argv[1:]
client=memo.Client(url,memo.load_key(Path(key)),timeout=10)
original=client.send
def lose_response(command):
    original(command)
    os._exit(73)
client.send=lose_response
outbox.Outbox(path,url,public).flush(client)
"""
                crashed = subprocess.run([sys.executable, "-c", code, str(path), origin, public, str(key_path)],
                    env={**os.environ, "PYTHONPATH": str(CLIENT_DIR)}, capture_output=True, timeout=30)
                self.assertEqual(crashed.returncode, 73)
                before = queue.inspect("process-crash", sensitive=True)
                recovered = subprocess.run([sys.executable, str(CLIENT_DIR / "swarmmemo_outbox.py"), "--db", str(path), "--url", origin,
                    "--public-key=" + public, "flush", "--key", str(key_path)], capture_output=True, timeout=30)
                self.assertEqual(recovered.returncode, 0, recovered.stderr.decode())
                after = queue.inspect("process-crash", sensitive=True)
                self.assertEqual(before["envelope"], after["envelope"])
                self.assertEqual(before["envelope_sha256"], after["envelope_sha256"])
                self.assertTrue(after["response"]["receipt"]["duplicate"])
                events = memo.Client(origin).messages(room="lobby", cursor="start")["messages"]
                self.assertEqual(sum(e["text"] == "process-crash-unique-canary" for e in events), 1)
                client = memo.Client(origin, memo.load_key(key_path), timeout=10)
                def deliver(intent_id, intent, **kwargs):
                    queue.enqueue(intent_id, intent)
                    result = queue.flush(client, **kwargs)
                    self.assertEqual(result[-1]["state"], "acknowledged", result)
                    return queue.inspect(intent_id, sensitive=True)["response"]
                # Exercise every current mutation's actual acknowledgement,
                # including successful omitted/zero defaults and normalized handles.
                deliver("register-default", {"operation": "agent.register"})
                deliver("register-name", {"operation": "agent.register", "handle": "OutboxAgent"})
                deliver("room-default", {"operation": "room.create", "room": "outbox-public"})
                deliver("room-private", {"operation": "room.create", "room": "outbox-private", "visibility": "private"})
                target_key = memo.crypto()[0].generate()
                target_id = outbox.sha(memo.public_bytes(target_key))
                memo.Client(origin, target_key).command("agent.register")
                deliver("member-add", {"operation": "room.member.add", "room": "outbox-private", "target": target_id})
                deliver("member-remove", {"operation": "room.member.remove", "room": "outbox-private", "target": target_id})
                body = b"outbox-private-attachment"
                blob = deliver("blob-defaults", {"operation": "blob.put", "room": "outbox-private", "data": memo.b64(body), "ttl": 0})["data"]["blob"]
                deliver("post-private", {"operation": "post", "room": "outbox-private", "text": "private default page", "attachments": [blob["id"]]})
                deliver("blob-delete", {"operation": "blob.delete", "message_id": blob["id"]})
                alias_blob = deliver("blob-alias", {"operation": "blob.put", "room": "lobby", "data": memo.b64(body)})["data"]["blob"]
                deliver("blob-delete-alias", {"operation": "blob.delete", "target": alias_blob["id"]})
                deliver("credit", {"operation": "credit.transfer", "target": target_id, "amount": 100})
                deliver("report", {"operation": "report", "message_id": after["response"]["receipt"]["id"], "reason": "Fixture report for acknowledgement validation"})
                lease = deliver("lease", {"operation": "lease.acquire", "room": "lobby", "target": "outbox-task", "ttl": 60})["data"]
                deliver("lease-release", {"operation": "lease.release", "room": "lobby", "target": "outbox-task", "amount": lease["fence"]})
                card = json.dumps({"schema": 1, "description": "Fixture outbox card", "capabilities": ["testing"], "availability": "available"})
                deliver("peer", {"operation": "agent.profile.publish", "data": card, "ttl": 0})
                deliver("peer-remove", {"operation": "agent.profile.remove"})
                successor = memo.crypto()[0].generate()
                deliver("rotation", {"operation": "agent.rotate", "target": memo.b64(memo.public_bytes(successor))}, rotation_key=successor)
            finally:
                process.terminate()
                try: process.wait(timeout=5)
                except subprocess.TimeoutExpired: process.kill(); process.wait()


if __name__ == "__main__": unittest.main()
