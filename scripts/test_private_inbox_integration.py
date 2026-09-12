"""Opt-in disposable Go -> signed worker -> private metadata ledger fixtures."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
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
from swarmmemo_private_inbox import PrivateRoomInbox, PrivateInboxError
import swarmmemo_private_transport as transport

BINARY = os.environ.get("SWARMMEMO_PRIVATE_INBOX_TEST_BINARY")
CANARY = "private-live-CANARY-☃-not-a-public-fixture-message"


@unittest.skipUnless(BINARY, "requires explicit disposable Go binary")
class ActualGoTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="swarmmemo-private-live-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.data = self.root / "server"
        self.data.mkdir(mode=0o700)
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        self.origin = "http://127.0.0.1:" + str(port)
        self.env = {**os.environ, "DATA_DIR": str(self.data), "LISTEN_ADDR": "127.0.0.1:" + str(port),
                    "PUBLIC_URL": self.origin, "ALLOW_INSECURE_LOCAL": "true"}
        self.server = None
        self.addCleanup(self.stop)
        self.start()
        self.owner = memo.Client(self.origin, memo.crypto()[0].generate(), timeout=5)
        self.reader_file = self.root / "reader.json"
        reader = memo.keygen(self.reader_file)
        self.reader_id = reader["id"]
        self.reader = memo.Client(self.origin, memo.load_key(self.reader_file), timeout=5)
        self.reader.command("agent.register")
        self.owner.command("room.create", room="private-a", visibility="private", members=[self.reader_id])
        self.owner.command("room.create", room="private-b", visibility="private", members=[self.reader_id])
        self.binding = {"schema": 1, "type": "private-room-inbox", "origin": self.origin, "service_id": "swarmmemo.com",
                        "room": "private-a", "reader_public_key": reader["public_key"], "start_mode": "history",
                        "storage": "metadata-only", "offline_bodies": "deny"}
        self.path = self.root / "inbox.sqlite"
        self.box = PrivateRoomInbox(self.path, self.binding)
        self.box.create(consent_private_metadata=True)
        self.box.add_consumer("planner")

    def start(self):
        self.server = subprocess.Popen([BINARY, "serve"], env=self.env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        for _ in range(100):
            try:
                with opener.open(self.origin + "/health", timeout=1) as response:
                    response.read(4096)
                return
            except OSError:
                time.sleep(.02)
        self.fail("disposable Go server did not start")

    def stop(self):
        if self.server is not None:
            self.server.terminate()
            try:
                self.server.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.server.kill()
                self.server.wait(timeout=5)
            self.server = None

    def post(self, room="private-a", text=CANARY, **fields):
        # Deliberately omit signed visibility: existing ordinary private posts
        # derive destination privacy from the room, not from author assertions.
        return self.owner.command("post", room=room, text=text, **fields)["receipt"]["id"]

    def poll(self):
        return self.box.poll(key_path=self.reader_file)

    def resync(self):
        for _ in range(20):
            result = self.box.resync(key_path=self.reader_file)
            if result["phase"] == "ready":
                return result
            self.assertEqual(result["last_error"], "request_budget", result)
        self.fail("resync failed to make progress")

    def test_real_two_room_scope_revoke_readd_and_reader_rotation(self):
        message_id = self.post()
        self.post("private-b", text="other-room-CANARY")
        self.assertEqual(self.poll()["phase"], "ready")
        item = self.box.pending("planner")[0]
        self.assertEqual(item["message_id"], message_id)
        self.assertEqual(len(self.box.pending("planner")), 1)
        read = self.box.read_current("planner", item["notification_id"], key_path=self.reader_file, disclose_private_body=True)
        self.assertEqual(read["message"]["text"], CANARY)
        self.box.ack("planner", item["notification_id"], item["snapshot_digest"])
        self.owner.command("room.member.remove", room="private-a", target=self.reader_id)
        self.assertEqual(self.poll()["phase"], "unavailable")
        self.owner.command("room.member.add", room="private-a", target=self.reader_id)
        with self.assertRaisesRegex(PrivateInboxError, "unavailable"):
            self.poll()
        self.resync()
        self.assertEqual(self.box.pending("planner"), [])
        successor_file = self.root / "successor.json"
        successor = memo.keygen(successor_file)
        self.reader.rotate(memo.load_key(successor_file))
        self.assertEqual(self.poll()["phase"], "reauth_required")
        self.assertEqual(self.box.resync(key_path=self.reader_file)["phase"], "reauth_required")
        new_box = PrivateRoomInbox(self.root / "successor.sqlite", {**self.binding, "reader_public_key": successor["public_key"]})
        new_box.create(consent_private_metadata=True)
        new_box.add_consumer("planner")
        self.assertEqual(new_box.poll(key_path=successor_file)["phase"], "ready")
        self.assertEqual(new_box.pending("planner")[0]["message_id"], message_id)
        self.assertEqual(self.box.pending("planner"), [])
        for path in (self.path, self.root / "successor.sqlite"):
            self.assertNotIn(CANARY.encode(), path.read_bytes())
            self.assertNotIn(b"other-room-CANARY", path.read_bytes())

    def test_real_backup_restore_new_generation_and_lost_event(self):
        first = self.post()
        self.poll()
        item = self.box.pending("planner")[0]
        self.box.ack("planner", item["notification_id"], item["snapshot_digest"])
        backup = self.root / "synthetic-private-backup.sqlite"
        subprocess.run([BINARY, "backup", str(backup)], env=self.env, check=True, capture_output=True, timeout=10)
        second = self.post(text=CANARY + "-second")
        self.poll()
        self.assertEqual(self.box.pending("planner")[0]["message_id"], second)
        self.stop()
        restored = self.root / "restored-server"
        restored.mkdir(mode=0o700)
        shutil.copyfile(backup, restored / "swarmmemo.db")
        self.env["DATA_DIR"] = str(restored)
        subprocess.run([BINARY, "recover-generation", "--offline-confirmed"], env=self.env,
                       check=True, capture_output=True, timeout=10)
        self.start()
        self.assertEqual(self.poll()["phase"], "resync_required")
        self.resync()
        pending = self.box.pending("planner")
        self.assertEqual(len(pending), 1)
        self.assertEqual((pending[0]["message_id"], pending[0]["state"]), (second, "unavailable"))
        with self.box.database() as db:
            self.assertEqual(db.execute("SELECT count(*) FROM acknowledgements").fetchone()[0], 1)
            self.assertEqual(db.execute("SELECT state FROM events WHERE id=?", (first,)).fetchone()[0], "live")

    def test_real_crashes_before_and_after_page_commit_are_metadata_only(self):
        self.post()
        runner = """
import json,os,sys
sys.path.insert(0,sys.argv[1])
import swarmmemo_private_inbox as m
box=m.PrivateRoomInbox(sys.argv[2],json.loads(sys.argv[3]))
if sys.argv[5]=='before':
    original=box._receive
    def crash(*a,**kw):
        original(*a,**kw)
        os._exit(83)
    box._receive=crash
else:
    original=box._status
    def crash(db,requests=0):
        if requests: os._exit(84)
        return original(db,requests)
    box._status=crash
box.poll(key_path=sys.argv[4])
"""
        args = [sys.executable, "-B", "-c", runner, str(CLIENTS), str(self.path), json.dumps(self.binding), str(self.reader_file)]
        result = subprocess.run(args + ["before"], capture_output=True, timeout=15)
        self.assertEqual(result.returncode, 83, result.stderr)
        self.assertEqual(self.box.status()["phase"], "new")
        self.assertEqual(self.box.pending("planner"), [])
        result = subprocess.run(args + ["after"], capture_output=True, timeout=15)
        self.assertEqual(result.returncode, 84, result.stderr)
        self.box = PrivateRoomInbox(self.path, self.binding)
        self.assertEqual(self.box.status()["phase"], "ready")
        self.assertEqual(len(self.box.pending("planner")), 1)
        self.poll()
        self.assertEqual(self.box.status()["notifications"], 1)
        self.assertNotIn(CANARY.encode(), self.path.read_bytes())

    def test_real_attachment_delete_expiry_and_moderator_latch(self):
        blob = self.owner.command("blob.put", room="private-a", data=memo.b64(b"private-binary-canary"),
                                  filename=CANARY, media_type="text/plain", ttl=60)["data"]["blob"]
        expiring = self.owner.command("blob.put", room="private-a", data=memo.b64(b"expiring-binary-canary"),
                                      filename=CANARY, ttl=20)["data"]["blob"]
        message_id = self.post(attachments=[blob["id"], expiring["id"]])
        actions = []
        original_fetch = transport.ReadSession._fetch
        def record(session, action, arguments):
            actions.append(action)
            return original_fetch(session, action, arguments)
        with patch.object(transport.ReadSession, "_fetch", record):
            self.assertEqual(self.poll()["phase"], "ready")
            first = self.box.pending("planner")[0]
            with self.box.database() as db:
                pinned = bytes(db.execute("SELECT immutable FROM events").fetchone()[0])
                meta = transport.strict_json(db.execute("SELECT metadata FROM events").fetchone()[0])
            self.assertFalse(meta["attachments"][1]["expired"])
            self.owner.command("blob.delete", message_id=blob["id"])
            with self.assertRaisesRegex(PrivateInboxError, "notification_superseded"):
                self.box.read_current("planner", first["notification_id"], key_path=self.reader_file, disclose_private_body=True)
            second = self.box.pending("planner")[0]
            self.assertNotEqual(second["snapshot_digest"], first["snapshot_digest"])
            with self.box.database() as db:
                self.assertEqual(bytes(db.execute("SELECT immutable FROM events").fetchone()[0]), pinned)
                after_delete = transport.strict_json(db.execute("SELECT metadata FROM events").fetchone()[0])
                self.assertTrue(after_delete["attachments"][0]["deleted"])
                # Keep deletion and expiry as two observed transitions. The old
                # five-second fixture could expire during the first read under
                # parallel-suite load, correctly merging both changes and then
                # falsely expecting a second notification supersession.
                self.assertFalse(after_delete["attachments"][1]["expired"],
                                 "fixture expiry preceded its separately tested transition")
            deadline = time.monotonic() + 30
            while int(time.time()) < expiring["expires_at"]:
                self.assertLess(time.monotonic(), deadline)
                time.sleep(.05)
            with self.assertRaisesRegex(PrivateInboxError, "notification_superseded"):
                self.box.read_current("planner", second["notification_id"], key_path=self.reader_file, disclose_private_body=True)
            third = self.box.pending("planner")[0]
            with self.box.database() as db:
                self.assertEqual(bytes(db.execute("SELECT immutable FROM events").fetchone()[0]), pinned)
                self.assertTrue(transport.strict_json(db.execute("SELECT metadata FROM events").fetchone()[0])["attachments"][1]["expired"])
            read = self.box.read_current("planner", third["notification_id"], key_path=self.reader_file, disclose_private_body=True)
            self.assertEqual(read["message"]["text"], CANARY)
            self.assertTrue(read["message"]["signature"])
            subprocess.run([BINARY, "moderate", message_id, "hide", CANARY], env=self.env, check=True, capture_output=True, timeout=10)
            self.assertEqual(self.poll()["phase"], "ready")
            removal = self.box.pending("planner")[0]
            self.assertEqual(removal["state"], "tombstoned")
            subprocess.run([BINARY, "moderate", message_id, "restore", "synthetic unhide"], env=self.env, check=True, capture_output=True, timeout=10)
            self.resync()
            self.assertEqual(self.box.pending("planner")[0]["notification_id"], removal["notification_id"])
            with self.assertRaisesRegex(PrivateInboxError, "body_unavailable"):
                self.box.read_current("planner", removal["notification_id"], key_path=self.reader_file, disclose_private_body=True)
        self.assertLessEqual(set(actions), {"capabilities", "room.get", "messages.list", "message.get"})
        for canary in (CANARY.encode(), b"private-binary-canary", b"expiring-binary-canary"):
            self.assertNotIn(canary, self.path.read_bytes())


if __name__ == "__main__":
    unittest.main()
