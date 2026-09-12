"""Synthetic real Go -> owner outbox -> private-grant inbox integration.

Run with PYTHONDONTWRITEBYTECODE=1 and an explicit disposable schema8 candidate:
SWARMMEMO_PRIVATE_READ_TEST_BINARY=/absolute/candidate python3 -B -m unittest
discover -s scripts -p test_private_read_integration.py. Never uses a supplied URL,
real source, production data directory, or public message mutation.
"""
import hashlib
import http.client
import json
import os
from pathlib import Path
import shutil
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.request
from unittest.mock import patch

CLIENTS = Path(__file__).resolve().parents[1] / "clients/python"
sys.path.insert(0, str(CLIENTS))
import swarmmemo as memo
import swarmmemo_outbox as outbox
from swarmmemo_private_inbox import PrivateRoomInbox, PrivateInboxError
import swarmmemo_private_transport as transport

BINARY = os.environ.get("SWARMMEMO_PRIVATE_READ_TEST_BINARY")
BODY = "PRIVATE-GRANT-BODY-CANARY-雪"
OTHER_BODY = "PRIVATE-GRANT-OTHER-ROOM-CANARY"


@unittest.skipUnless(BINARY, "requires explicit disposable private-read Go candidate")
class PrivateReadIntegration(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="swarmmemo-private-read-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.operator = self.root / "operator"
        self.operator.mkdir(mode=0o700)
        self.data = self.root / "server"
        self.data.mkdir(mode=0o700)
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            self.port = listener.getsockname()[1]
        self.origin = "http://127.0.0.1:" + str(self.port)
        self.env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "DATA_DIR": str(self.data),
                    "LISTEN_ADDR": "127.0.0.1:" + str(self.port), "PUBLIC_URL": self.origin,
                    "SERVICE_ID": "swarmmemo.com", "ALLOW_INSECURE_LOCAL": "true",
                    "ARCHIVE_DELAY_SECONDS": "0", "PYTHONDONTWRITEBYTECODE": "1"}
        self.binary = str(Path(BINARY).resolve(strict=True))
        self.server = None
        self.addCleanup(self.stop)
        self.start()
        self.owner_key = memo.crypto()[0].generate()
        self.owner = self.client(self.owner_key)
        self.member_key = memo.crypto()[0].generate()
        self.member = self.client(self.member_key)
        self.nonmember = self.client(memo.crypto()[0].generate())
        for client in (self.owner, self.member, self.nonmember):
            client.command("agent.register")
        self.member_id = self.key_id(self.member_key)
        self.owner.command("room.create", room="read-a", visibility="private", members=[self.member_id])
        self.owner.command("room.create", room="read-b", visibility="private", members=[self.member_id])
        self.queue = outbox.Outbox(self.operator / "owner-outbox.sqlite", self.origin,
                                  memo.b64(memo.public_bytes(self.owner_key)))
        self.native_before = self.get_json("/api/stats")["stats"]

    @staticmethod
    def key_id(key):
        return hashlib.sha256(memo.public_bytes(key)).hexdigest()

    def client(self, key):
        client = memo.Client(self.origin, key, timeout=5)
        client.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), memo.NoRedirect())
        return client

    def start(self):
        self.server = subprocess.Popen([self.binary, "serve"], env=self.env,
                                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            if self.server.poll() is not None:
                self.fail("isolated private-read candidate exited before readiness")
            try:
                if self.http("/health")[0] == 200:
                    return
            except OSError:
                time.sleep(.025)
        self.fail("isolated private-read candidate did not become ready")

    def stop(self):
        if self.server is not None:
            if self.server.poll() is None:
                self.server.terminate()
                try:
                    self.server.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    self.server.kill()
                    self.server.wait(timeout=5)
            self.server = None

    def http(self, path, body=None):
        self.assertTrue(path.startswith("/") and not path.startswith("//"))
        connection = http.client.HTTPConnection("127.0.0.1", self.port, timeout=5)
        try:
            connection.request("GET" if body is None else "POST", path,
                               body=None if body is None else json.dumps(body).encode(),
                               headers={"Content-Type": "application/json"})
            response = connection.getresponse()
            raw = response.read((1 << 20) + 1)
            self.assertLessEqual(len(raw), 1 << 20)
            return response.status, raw
        finally:
            connection.close()

    def get_json(self, path):
        status, raw = self.http(path)
        self.assertEqual(status, 200, raw[:256])
        return json.loads(raw)

    def control_state(self, room="read-a", owner=None):
        return (owner or self.owner).command("private_read.list", room=room)["data"]

    def enroll(self, name, *, room="read-a", uncertain=False):
        host = self.root / ("reader-" + name)
        host.mkdir(mode=0o700)
        key_path = host / "child.json"
        child = memo.keygen(key_path)
        key = memo.load_key(key_path)
        state = self.control_state(room)
        intent = memo.private_read_enrollment_intent(child["public_key"], room=room,
            generation=state["generation"], access_epoch=state["access_epoch"])
        intent_id = "enroll-" + name
        self.queue.enqueue(intent_id, intent)
        if uncertain:
            original = self.owner.send

            def lose_ack(command):
                original(command)  # Real server commits before the synthetic loss.
                raise OSError("synthetic lost response")

            with patch.object(self.owner, "send", side_effect=lose_ack):
                self.assertEqual(self.queue.flush(self.owner, 1, target_key=key)[0]["state"], "unresolved")
            saved = self.queue.inspect(intent_id, sensitive=True)["envelope"]
            self.assertTrue(saved["proof"])
            # A prepared retry needs no child private key on the operator machine.
            self.assertEqual(self.queue.flush(self.owner, 1)[0]["state"], "acknowledged")
            self.assertEqual(self.queue.inspect(intent_id, sensitive=True)["envelope"], saved)
        else:
            self.assertEqual(self.queue.flush(self.owner, 1, target_key=key)[0]["state"], "acknowledged")
        retained = self.queue.inspect(intent_id, sensitive=True)
        ack = retained["response"]["data"]["ack"]
        self.assertEqual(ack["type"], "private-room-read-grant-ack")
        self.assertTrue(ack["historical_acknowledgement"])
        self.assertEqual((ack["grant_id"], ack["child_id"]), (child["id"], child["id"]))
        self.assertEqual(ack["expires_at"] - ack["accepted_at"], 86400)
        binding = {"schema": 2, "type": "private-room-grant-inbox", "origin": self.origin,
                   "service_id": "swarmmemo.com", "room": room, "reader_public_key": child["public_key"],
                   "start_mode": "history", "storage": "metadata-only", "offline_bodies": "deny",
                   "private_read": {"schema": 1, "grant_id": child["id"], "generation": ack["grant_generation"]}}
        box = PrivateRoomInbox(host / "inbox.sqlite", binding)
        box.create(consent_private_metadata=True)
        for consumer in ("planner", "reviewer"):
            box.add_consumer(consumer)
        return {"name": name, "host": host, "key_path": key_path, "key": key, "child": child,
                "binding": binding, "box": box, "envelope": retained["envelope"], "ack": ack}

    def child_wire(self, grant, operation, **fields):
        return memo.sign({"operation": operation, "room": grant["binding"]["room"],
                          **fields, "private_read": grant["binding"]["private_read"]}, grant["key"])

    def post(self, room="read-a", text=BODY, **fields):
        return self.owner.command("post", room=room, text=text, **fields)["receipt"]["id"]

    def revoke(self, grant, *, owner=None, queue=None):
        owner = owner or self.owner
        queue = queue or self.queue
        room = grant["binding"]["room"]
        generation = self.control_state(room, owner)["generation"]
        intent_id = "revoke-" + grant["name"]
        queue.enqueue(intent_id, memo.private_read_revoke_intent(grant["child"]["id"], room=room, generation=generation))
        self.assertEqual(queue.flush(owner, 1)[0]["state"], "acknowledged")
        return queue.inspect(intent_id, sensitive=True)["response"]["data"]["ack"]

    def sql(self, query, arguments=()):
        connection = sqlite3.connect((Path(self.env["DATA_DIR"]) / "swarmmemo.db").as_uri() + "?mode=ro", uri=True)
        try:
            return connection.execute(query, arguments).fetchall()
        finally:
            connection.close()

    def assert_isolation(self, *grants):
        self.assertEqual(self.get_json("/api/stats")["stats"], self.native_before)
        self.assertFalse(self.get_json("/api/messages").get("messages"))
        self.assertFalse(self.get_json("/api/works")["data"].get("works"))
        self.assertEqual(self.http("/v1/export"), (200, b""))
        for grant in grants:
            child_id = grant["child"]["id"]
            self.assertEqual(self.sql("SELECT count(*) FROM identities WHERE id=?", (child_id,)), [(0,)])
            self.assertEqual(self.sql("SELECT count(*) FROM members WHERE account=?", (child_id,)), [(0,)])
            self.assertEqual(self.sql("SELECT count(*) FROM quota WHERE actor=?", (child_id,)), [(0,)])
            public = self.get_json("/api/agents")
            self.assertNotIn(child_id.encode(), json.dumps(public).encode())
            self.assertEqual(self.http("/api/delegation/" + child_id)[0], 404)
            for path in grant["host"].iterdir():
                if path.name == "child.json":
                    continue
                raw = path.read_bytes()
                for forbidden in (BODY.encode(), OTHER_BODY.encode(), b"signed_payload",
                                  grant["envelope"]["proof"].encode(), grant["envelope"]["signature"].encode()):
                    self.assertNotIn(forbidden, raw, path.name)

    def test_two_consumers_fresh_body_exact_retry_and_minimal_scope(self):
        message_id = self.post()
        self.post("read-b", OTHER_BODY)
        grant = self.enroll("two-consumers", uncertain=True)
        wire = self.child_wire(grant, "room.get")
        status, raw = self.http("/v1/command", wire)
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(raw), {"ok": True, "data": {"private_room": {"name": "read-a", "visibility": "private"}}})
        actions = []
        original_fetch = transport.ReadSession._fetch

        def record(session, operation, fields):
            actions.append(operation)
            return original_fetch(session, operation, fields)

        with patch.object(transport.ReadSession, "_fetch", record):
            self.assertEqual(grant["box"].poll(key_path=grant["key_path"])["phase"], "ready")
            first = grant["box"].pending("planner")[0]
            self.assertEqual(first["message_id"], message_id)
            self.assertEqual(grant["box"].pending("reviewer"), [first])
            for consumer in ("planner", "reviewer"):
                read = grant["box"].read_current(consumer, first["notification_id"],
                    key_path=grant["key_path"], disclose_private_body=True)
                self.assertEqual(read["message"]["text"], BODY)
                self.assertEqual(json.loads(read["message"]["signed_payload"])["version"], 1)
                self.assertNotIn("delegation_id", read["message"])
            grant["box"].ack("planner", first["notification_id"], first["snapshot_digest"])
            restarted = PrivateRoomInbox(grant["host"] / "inbox.sqlite", grant["binding"])
            self.assertEqual(restarted.pending("planner"), [])
            self.assertEqual(restarted.pending("reviewer"), [first])
        self.assertLessEqual(set(actions), {"capabilities", "room.get", "messages.list", "message.get"})
        for command in (self.child_wire(grant, "message.get", room="read-b", message_id=message_id),
                        self.child_wire(grant, "post", text="MUST-NOT-POST"),
                        self.child_wire(grant, "private_read.get", target=grant["child"]["id"]),
                        memo.sign({"operation": "agent.register"}, grant["key"])):
            status, raw = self.http("/v1/command", command)
            self.assertEqual(status, 404)
            self.assertEqual(json.loads(raw)["error"]["code"], "not_found")
            self.assertNotIn(BODY.encode(), raw)
        with patch.object(transport, "ReadSession", side_effect=AssertionError("offline inspection performed network")):
            self.assertEqual(restarted.status()["network_requests"], 0)
            restarted.pending("reviewer")
        self.assert_isolation(grant)

    def test_empty_initial_and_idle_pages_preserve_schema2_checkpoint(self):
        grant = self.enroll("empty-idle")
        for _ in range(2):
            self.assertEqual(grant["box"].poll(key_path=grant["key_path"])["phase"], "ready")
            self.assertEqual(grant["box"].pending("planner"), [])
        with grant["box"].database() as db:
            cursor = db.execute("SELECT cursor FROM checkpoint").fetchone()[0]
            self.assertTrue(cursor)
        message_id = self.post()
        self.assertEqual(grant["box"].poll(key_path=grant["key_path"])["phase"], "ready")
        self.assertEqual(grant["box"].pending("planner")[0]["message_id"], message_id)
        self.assert_isolation(grant)

    def test_revoke_freezes_fresh_body_and_does_not_reactivate_catalog(self):
        self.post()
        grant = self.enroll("revoke")
        self.assertEqual(grant["box"].poll(key_path=grant["key_path"])["phase"], "ready")
        first = grant["box"].pending("planner")[0]
        accepted = self.revoke(grant)
        self.assertEqual(accepted["state"], "revoked")
        with self.assertRaises(PrivateInboxError):
            grant["box"].read_current("planner", first["notification_id"],
                key_path=grant["key_path"], disclose_private_body=True)
        self.assertEqual(grant["box"].status()["phase"], "unavailable")
        self.assertEqual(grant["box"].resync(key_path=grant["key_path"])["phase"], "unavailable")
        self.assertEqual(self.owner.send(grant["envelope"])["data"]["ack"], grant["ack"])
        self.assertEqual(self.http("/v1/command", self.child_wire(grant, "room.get"))[0], 404)
        replacement = self.enroll("replacement")
        with self.assertRaisesRegex(PrivateInboxError, "binding_mismatch"):
            PrivateRoomInbox(grant["host"] / "inbox.sqlite", replacement["binding"]).status()
        self.assertEqual(replacement["box"].poll(key_path=replacement["key_path"])["phase"], "ready")
        self.assert_isolation(grant, replacement)

    def test_actual_member_remove_not_noop_invalidates_all_readers_once(self):
        first = self.enroll("epoch-first")
        second = self.enroll("epoch-second")
        before = self.control_state()["access_epoch"]
        self.owner.command("room.member.remove", room="read-a", target=self.key_id(self.nonmember.key))
        self.assertEqual(self.control_state()["access_epoch"], before)
        for grant in (first, second):
            self.assertEqual(self.http("/v1/command", self.child_wire(grant, "room.get"))[0], 200)
        remove = self.owner.prepare("room.member.remove", room="read-a", target=self.member_id, request_id="remove-member-once")
        self.owner.send(remove)
        after = self.control_state()["access_epoch"]
        self.assertNotEqual(after, before)
        self.owner.send(remove)
        self.assertEqual(self.control_state()["access_epoch"], after)
        self.owner.command("room.member.add", room="read-a", target=self.member_id)
        for grant in (first, second):
            self.assertEqual(self.http("/v1/command", self.child_wire(grant, "room.get"))[0], 404)
            state = self.owner.command("private_read.get", room="read-a", target=grant["child"]["id"])["data"]
            self.assertEqual(state["private_read_grant"]["state"], "room_epoch_disabled")
        self.assert_isolation(first, second)

    def test_issuer_rotation_successor_revoke_and_public_profile_silence(self):
        owner_id = self.key_id(self.owner_key)
        before = self.sql("SELECT id,account,last_seen FROM identities ORDER BY id")
        audits = self.sql("SELECT count(*) FROM audit")
        grant = self.enroll("issuer")
        self.owner.command("private_read.get", room="read-a", target=grant["child"]["id"])
        self.assertEqual(self.sql("SELECT id,account,last_seen FROM identities ORDER BY id"), before)
        self.assertEqual(self.sql("SELECT count(*) FROM audit"), audits)
        successor_key = memo.crypto()[0].generate()
        self.owner.rotate(successor_key)
        successor = self.client(successor_key)
        state = successor.command("private_read.get", room="read-a", target=grant["child"]["id"])["data"]
        self.assertEqual(state["private_read_grant"]["state"], "issuer_rotated")
        self.assertEqual(state["private_read_grant"]["issuer_id"], owner_id)
        self.assertEqual(self.http("/v1/command", self.child_wire(grant, "room.get"))[0], 404)
        queue = outbox.Outbox(self.operator / "successor-outbox.sqlite", self.origin,
                             memo.b64(memo.public_bytes(successor_key)))
        self.assertEqual(self.revoke(grant, owner=successor, queue=queue)["state"], "revoked")
        self.assert_isolation(grant)

    def test_older_backup_lost_enrollment_and_restored_revocation_are_epoch_denied(self):
        self.post()
        retained = self.enroll("retained")
        self.assertEqual(retained["box"].poll(key_path=retained["key_path"])["phase"], "ready")
        notice = retained["box"].pending("planner")[0]
        retained["box"].ack("planner", notice["notification_id"], notice["snapshot_digest"])
        backup = self.root / "before-revoke.sqlite"
        subprocess.run([self.binary, "backup", str(backup)], env=self.env,
                       check=True, capture_output=True, timeout=10)
        self.revoke(retained)
        lost = self.enroll("lost-after-backup")
        self.assertEqual(self.http("/v1/command", self.child_wire(lost, "room.get"))[0], 200)
        old_generation = retained["binding"]["private_read"]["generation"]
        self.stop()
        restored = self.root / "restored"
        restored.mkdir(mode=0o700)
        shutil.copyfile(backup, restored / "swarmmemo.db")
        (restored / "swarmmemo.db").chmod(0o600)
        self.env["DATA_DIR"] = str(restored)
        subprocess.run([self.binary, "recover-generation", "--offline-confirmed"], env=self.env,
                       check=True, capture_output=True, timeout=10)
        self.start()
        self.assertNotEqual(self.control_state()["generation"], old_generation)
        for grant in (retained, lost):
            self.assertEqual(self.http("/v1/command", self.child_wire(grant, "room.get"))[0], 404)
        record = self.owner.command("private_read.get", room="read-a", target=retained["child"]["id"])["data"]["private_read_grant"]
        self.assertEqual((record["state"], record["revoked_at"]), ("epoch_disabled", 0))
        self.assertEqual(self.http("/v1/command", self.owner.prepare("private_read.get", room="read-a", target=lost["child"]["id"]))[0], 404)
        self.assertEqual(self.owner.send(retained["envelope"])["data"]["ack"], retained["ack"])
        self.assertEqual(retained["box"].poll(key_path=retained["key_path"])["phase"], "unavailable")
        self.assertEqual(retained["box"].resync(key_path=retained["key_path"])["phase"], "unavailable")
        with retained["box"].database() as db:
            self.assertEqual(db.execute("SELECT count(*) FROM acknowledgements").fetchone()[0], 1)
        fresh = self.enroll("restored-new")
        self.assertEqual(fresh["box"].poll(key_path=fresh["key_path"])["phase"], "ready")
        self.assert_isolation(retained, lost, fresh)


if __name__ == "__main__":
    unittest.main()
