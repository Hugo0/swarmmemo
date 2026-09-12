"""Optional real Go server integration: set SWARMMEMO_TEST_BINARY to a built server."""
import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parent))
import publish_hf as publisher
import swarmmemo as client


@unittest.skipUnless(os.environ.get("SWARMMEMO_TEST_BINARY"), "set SWARMMEMO_TEST_BINARY for Go integration")
class LiveClientTests(unittest.TestCase):
    def test_identity_permissions_retries_rotation_and_export(self):
        binary = os.environ["SWARMMEMO_TEST_BINARY"]
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            with socket.socket() as reservation:
                reservation.bind(("127.0.0.1", 0)); port = reservation.getsockname()[1]
            token = "disposable-integration-admin-token-0123456789"
            token_file = work / "admin"; token_file.write_text(token); token_file.chmod(0o600)
            env = {**os.environ, "DATA_DIR": str(work / "data"), "LISTEN_ADDR": "127.0.0.1:" + str(port),
                   "ALLOW_INSECURE_LOCAL": "true", "ARCHIVE_DELAY_SECONDS": "1", "ADMIN_TOKEN_FILE": str(token_file)}
            process = subprocess.Popen([binary, "serve"], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            origin = "http://127.0.0.1:" + str(port)
            try:
                for _ in range(100):
                    try:
                        with urllib.request.urlopen(origin + "/health", timeout=1): break
                    except OSError:
                        if process.poll() is not None: self.fail("server exited during startup")
                        time.sleep(0.05)
                else: self.fail("server did not start")

                # Both implementations read the same protected seed-based key format.
                go_key = work / "go-key.json"
                subprocess.run([binary, "keygen", str(go_key)], check=True, stdout=subprocess.DEVNULL)
                alice_key = client.load_key(go_key)
                alice = client.Client(origin, alice_key)
                bob_file = work / "bob.json"; client.keygen(bob_file)
                bob_key = client.load_key(bob_file); bob = client.Client(origin, bob_key)
                bob_id = hashlib.sha256(client.public_bytes(bob_key)).hexdigest()
                anon = client.Client(origin)

                vector = json.loads((Path(__file__).parents[1] / "clients/python/signing-vector.json").read_bytes())
                canonical = subprocess.run([binary, "canonical"], input=json.dumps(vector["command"]).encode(), check=True, capture_output=True).stdout
                self.assertEqual(canonical, vector["canonical"].encode())

                alice.command("agent.register", handle="alice")
                bob.command("agent.register", handle="bob")
                self.assertEqual(anon.command("agent.get", target=bob_id)["identity"]["handle"], "bob")
                for mode in ("get", "base64"):
                    receipt = anon.post("lobby", "main", "Anonymous <🌍> " + mode, request_id="anon-" + mode, transport=mode)
                    self.assertTrue(receipt["ok"])
                prepared = alice.prepare("post", room="lobby", page="main", text="signed <🌍>\u2028", request_id="signed-one")
                sent = alice.send(prepared)
                same = alice.send(prepared)
                self.assertEqual(sent["receipt"]["id"], same["receipt"]["id"])
                self.assertTrue(same["receipt"]["duplicate"])
                c64_post = alice.post("lobby", "main", "Signed path-only command", transport="c64")
                self.assertTrue(c64_post["ok"])
                bob.command("report", message_id=sent["receipt"]["id"], reason="integration-review-only")
                self.assertFalse(anon.command("message.get", message_id=sent["receipt"]["id"])["messages"][0]["hidden"])

                alice.command("room.create", room="private-room", visibility="private")
                private_post = alice.post("private-room", "main", "private-never-in-archive")
                with self.assertRaises(client.APIError): anon.messages("private-room")
                with self.assertRaises(client.APIError): bob.messages("private-room")
                alice.command("room.member.add", room="private-room", target=bob_id)
                self.assertEqual(bob.messages("private-room")["messages"][0]["text"], "private-never-in-archive")
                source = work / "attachment.bin"; source.write_bytes(bytes(range(256)))
                private_blob = alice.upload("private-room", source, ttl=3600)["data"]["blob"]
                private_attached = alice.post("private-room", "main", "Private attachment", attachments=[private_blob["id"]])
                with self.assertRaises(client.APIError): anon.command("blob.get", message_id=private_blob["id"])
                public_blob = alice.upload("lobby", source, ttl=3600)["data"]["blob"]
                public_attached = alice.post("lobby", "main", "Public attachment metadata", attachments=[public_blob["id"]])
                destination = work / "downloaded.bin"
                anon.download(public_blob["id"], destination)
                self.assertEqual(destination.read_bytes(), source.read_bytes())
                with self.assertRaises(FileExistsError): anon.download(public_blob["id"], destination)
                alice.command("room.member.remove", room="private-room", target=bob_id)
                with self.assertRaises(client.APIError): bob.messages("private-room")

                before = bob.command("quota.get")["data"]["remaining_bytes"]
                alice.command("credit.transfer", target=bob_id, amount=4096, request_id="credit-one")
                self.assertEqual(bob.command("quota.get")["data"]["remaining_bytes"], before + 4096)
                lease = alice.command("lease.acquire", room="private-room", target="work", ttl=30)
                alice.command("lease.release", room="private-room", target="work", amount=lease["data"]["fence"])

                new_file = work / "new.json"; client.keygen(new_file); new_key = client.load_key(new_file)
                rotated = alice.rotate(new_key)
                new_client = client.Client(origin, new_key)
                self.assertTrue(rotated["data"]["quota_preserved"])
                self.assertTrue(new_client.messages("private-room")["messages"])
                with self.assertRaises(client.APIError): alice.command("quota.get")
                history = new_client.command("messages.list", target=rotated["data"]["agent_id"])
                self.assertIn(sent["receipt"]["id"], [e["id"] for e in history["messages"]])

                # Poll boundedly for configured 1-second archival eligibility.
                for _ in range(30):
                    records, cursor = publisher.fetch(origin, "", int(time.time()))
                    if len(records) >= 5: break
                    time.sleep(0.1)
                self.assertEqual(len(records), 5)
                self.assertNotIn(private_post["receipt"]["id"], [row["id"] for row in records])
                self.assertNotIn(private_attached["receipt"]["id"], [row["id"] for row in records])
                attachment_row = next(row for row in records if row["id"] == public_attached["receipt"]["id"])
                self.assertEqual(attachment_row["attachments"][0]["id"], public_blob["id"])
                self.assertNotIn("data", attachment_row["attachments"][0])
                signed_row = next(row for row in records if row["id"] == sent["receipt"]["id"])
                publisher.validate(signed_row, int(time.time()))

                # Moderator removals enter export immediately and clear prior signed payload.
                removal = urllib.request.Request(origin + "/admin/moderate", data=json.dumps(
                    {"message_id": sent["receipt"]["id"], "reason": "integration-removal", "hide": True}).encode(),
                    headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
                with urllib.request.urlopen(removal) as response: self.assertEqual(response.status, 200)
                changes, updated = publisher.fetch(origin, cursor, int(time.time()))
                self.assertEqual(len(changes), 1)
                self.assertEqual(changes[0]["type"], "tombstone")
                self.assertFalse(changes[0].get("signed_payload"))
                self.assertNotEqual(cursor, updated)
                new_client.command("blob.delete", message_id=public_blob["id"])
                with self.assertRaises(client.APIError): anon.command("blob.get", message_id=public_blob["id"])
                blob_changes, _ = publisher.fetch(origin, updated, int(time.time()))
                self.assertEqual(len(blob_changes), 1)
                self.assertTrue(blob_changes[0]["attachments"][0]["deleted"])
            finally:
                process.terminate()
                try: process.wait(timeout=5)
                except subprocess.TimeoutExpired: process.kill(); process.wait()


if __name__ == "__main__": unittest.main()
