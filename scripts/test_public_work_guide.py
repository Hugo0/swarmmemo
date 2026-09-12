"""Exact public guide CLI commands; fresh processes and synthetic loopback only."""
import hashlib
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import shlex
import socket
import subprocess
import sys
import threading
import time
import unittest
from unittest.mock import patch

import test_private_read_integration as fixture_module


BINARY = os.environ.get("SWARMMEMO_TEST_BINARY")
ROOT = Path(__file__).resolve().parents[1]
GUIDE = ROOT / "clients/python/FIRST_PUBLIC_WORK.md"
EVIDENCE = "Synthetic local guide evidence; no external work performed. 雪"


@unittest.skipUnless(BINARY, "requires explicit disposable Go binary")
class PublicWorkGuide(unittest.TestCase):
    def setUp(self):
        self.f = fixture_module.PrivateReadIntegration()
        self.addCleanup(self.f.doCleanups)
        with patch.object(fixture_module, "BINARY", BINARY):
            self.f.setUp()
        self.guide = GUIDE.read_text(encoding="utf-8")
        self.commands = dict(re.findall(r"<!-- command: ([a-z-]+) -->\n```sh\n(.*?)\n```", self.guide, re.S))
        self.intents = dict(re.findall(r"<!-- intent: ([a-z-]+) -->\n```json\n(.*?)\n```", self.guide, re.S))
        self.assertEqual(len(self.commands), 12)
        self.assertEqual(set(self.intents), {"claim", "result", "submit", "accept"})
        self.worker = self.f.root / "worker"
        self.requester = self.f.root / "requester"
        self.worker.mkdir(mode=0o700)
        self.requester.mkdir(mode=0o700)
        self.child = fixture_module.memo.keygen(self.worker / "child.json")
        self.child_key = fixture_module.memo.load_key(self.worker / "child.json")
        self.requester_info = fixture_module.memo.keygen(self.requester / "requester.json")
        self.requester_client = self.f.client(fixture_module.memo.load_key(self.requester / "requester.json"))
        self.room = "synthetic-guide-lab"
        self.f.owner.command("room.create", room=self.room, visibility="public")
        self.generation = self.f.get_json("/api/changes?after=-1")["generation"]
        operations = ["post", "work.claim", "work.renew", "work.submit"]
        envelope = self.f.owner.prepare_enrollment(self.child_key, room=self.room, ttl=3600,
            amount=65536, generation=self.generation, operations=operations)
        self.f.owner.send(envelope)
        self.context = {"schema": 1, "grant_id": self.child["id"], "generation": self.generation}
        self.received = []
        self.observed_gets = []
        self.drop_after = False
        self.drop_before = False
        fixture = self

        class Relay(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                assert self.path in ("/capabilities", "/api/delegation/" + fixture.child["id"])
                fixture.observed_gets.append(self.path)
                status, body = fixture.f.http(self.path)
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_POST(self):
                size = int(self.headers.get("Content-Length", "0"))
                assert self.path == "/v1/command" and 0 < size <= 2 << 20
                raw = self.rfile.read(size)
                fixture.received.append(raw)
                if fixture.drop_before:
                    fixture.drop_before = False
                    self.close_connection = True
                    self.connection.shutdown(socket.SHUT_RDWR)
                    return
                upstream = http.client.HTTPConnection("127.0.0.1", fixture.f.port, timeout=5)
                try:
                    upstream.request("POST", self.path, raw, {"Content-Type": "application/json"})
                    result = upstream.getresponse()
                    body = result.read((1 << 20) + 1)
                    assert len(body) <= 1 << 20
                    if fixture.drop_after:
                        fixture.drop_after = False
                        assert result.status == 200
                        self.close_connection = True
                        self.connection.shutdown(socket.SHUT_RDWR)
                        return
                    self.send_response(result.status)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                finally:
                    upstream.close()

        self.relay = ThreadingHTTPServer(("127.0.0.1", 0), Relay)
        self.relay.daemon_threads = True
        self.relay_thread = threading.Thread(target=self.relay.serve_forever, daemon=True)
        self.relay_thread.start()
        self.addCleanup(self.stop_relay)
        self.origin = "http://127.0.0.1:" + str(self.relay.server_port)
        self.values = {"PYTHON": sys.executable, "ORIGIN": self.origin,
            "ROOM": self.room, "CHILD_PUBLIC_KEY": self.child["public_key"],
            "CHILD_FINGERPRINT": self.child["id"], "GRANT_GENERATION": self.generation,
            "GRANT_CONTEXT": json.dumps(self.context, separators=(",", ":")),
            "REQUESTER_PUBLIC_KEY": self.requester_info["public_key"],
            "AUTHORIZED_PUBLIC_EVIDENCE": EVIDENCE,
            "/absolute/worker": str(self.worker), "/absolute/requester": str(self.requester)}
        self.env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "PYTHONDONTWRITEBYTECODE": "1",
                    "NO_PROXY": "*", "no_proxy": "*"}

    def stop_relay(self):
        self.relay.shutdown()
        self.relay.server_close()
        self.relay_thread.join(timeout=3)
        self.assertFalse(self.relay_thread.is_alive())

    def replace(self, text):
        # Simultaneous placeholder substitution cannot rewrite inserted values.
        pattern = "|".join(re.escape(key) for key in sorted(self.values, key=len, reverse=True))
        return re.sub(pattern, lambda match: self.values[match.group()], text)

    def public_fetch(self, placeholder):
        self.assertIn("`" + placeholder + "`", self.guide)
        url = self.replace(placeholder)
        self.assertTrue(url.startswith(self.origin + "/"))
        connection = http.client.HTTPConnection("127.0.0.1", self.relay.server_port, timeout=5)
        try:
            connection.request("GET", url[len(self.origin):])
            result = connection.getresponse()
            self.assertEqual(result.status, 200)
            raw = result.read((1 << 20) + 1)
            self.assertLessEqual(len(raw), 1 << 20)
            return json.loads(raw)
        finally:
            connection.close()

    def capabilities_check(self):
        caps = self.public_fetch("ORIGIN/capabilities")
        self.assertEqual(caps["service_id"], "swarmmemo.com")
        self.assertIn(2, caps["canonical_versions"])
        self.assertEqual(caps["work_coordination"]["schema"], 1)
        self.assertTrue(caps["work_coordination"]["signed_transitions"])
        self.assertTrue(caps["work_coordination"]["generation_bound"])
        self.assertFalse(caps["work_coordination"]["automatic_execution"])
        self.assertEqual((caps["delegation"]["schema"], caps["delegation"]["canonical_version"],
                          caps["delegation"]["room_visibility"]), (1, 2, "public"))

    def grant_check(self, expected="active"):
        grant = self.public_fetch("ORIGIN/api/delegation/CHILD_FINGERPRINT")["data"]["delegation"]
        self.assertEqual((grant["service_id"], grant["generation"], grant["grant_id"], grant["public_key"],
                          grant["room"], grant["disclosure"], grant["state"]),
                         ("swarmmemo.com", self.generation, self.child["id"], self.child["public_key"],
                          self.room, "public", expected))
        self.assertEqual(set(grant["operations"]), {"post", "work.claim", "work.renew", "work.submit"})
        if expected == "active":
            self.assertGreater(grant["expires_at"], int(time.time()))
        return grant

    def command(self, name, expected=0, **overrides):
        values = self.values.copy()
        self.values.update(overrides)
        try:
            argv = [self.replace(part) for part in shlex.split(self.commands[name])]
        finally:
            self.values = values
        self.assertEqual(argv[0], sys.executable)
        self.assertIn(argv[1], ("clients/python/swarmmemo.py", "clients/python/swarmmemo_outbox.py"))
        self.assertEqual(argv[argv.index("--url") + 1], self.origin)
        if argv[1].endswith("swarmmemo_outbox.py"):
            self.assertTrue(any(arg.startswith("--public-key=") for arg in argv))
            self.assertNotIn("--public-key", argv)
        result = subprocess.run(argv, cwd=ROOT, env=self.env, capture_output=True, timeout=15)
        self.assertEqual(result.returncode, expected, result.stderr.decode())
        raw = result.stdout if result.stdout else result.stderr
        decoded = json.loads(raw)
        if name not in ("discover", "read-work", "read-brief", "read-result"):
            for forbidden in (b"private_key", b"signed_payload", b'"signature"', EVIDENCE.encode()):
                self.assertNotIn(forbidden, result.stdout + result.stderr)
        return decoded

    def enqueue(self, name):
        raw = self.replace(self.intents[name]).encode()
        json.loads(raw)
        directory = self.requester if name == "accept" else self.worker
        fd = os.open(directory / (name + ".json"), os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        with os.fdopen(fd, "wb") as stream:
            stream.write(raw)
            stream.flush()
            os.fsync(stream.fileno())
        result = self.command(name + "-enqueue")
        self.values[name.upper() + "_DIGEST"] = result["intent_sha256"]
        return result

    def seed_request(self):
        brief = self.requester_client.command("post", room=self.room, kind="simulation",
            text="Synthetic operator-owned guide fixture, not a real available job.")
        self.values["WORK_ID"] = brief["receipt"]["id"]
        self.requester_client.command("work.create", message_id=self.values["WORK_ID"], ttl=3600,
            data=json.dumps({"schema": 1, "generation": self.generation, "title": "Synthetic guide fixture", "capabilities": []}))

    def stored(self):
        return self.f.sql("SELECT work_id,sequence,operation,fence FROM work_transitions ORDER BY work_id,sequence"), self.f.sql("SELECT actor,day,used,incoming FROM quota ORDER BY actor,day"), self.f.sql("SELECT child_id,used_bytes FROM delegations ORDER BY child_id")

    def test_exact_documented_cli_lifecycle_lost_ack_and_target_selection(self):
        self.capabilities_check()
        self.assertFalse(self.command("discover")["data"].get("works"))
        self.assertNotIn(b"signature", self.received[-1])
        self.seed_request()
        self.assertEqual(self.command("discover")["data"]["works"][0]["id"], self.values["WORK_ID"])
        self.assertTrue(self.command("read-work")["data"]["work"]["simulated"])
        self.assertEqual(self.command("read-brief")["messages"][0]["id"], self.values["WORK_ID"])
        self.grant_check()
        self.enqueue("claim")
        self.enqueue("result")
        before = len(self.received)
        self.assertEqual(self.command("claim-deliver", 1, CLAIM_DIGEST="0" * 64)["error"], "intent_digest_mismatch")
        self.assertEqual(self.command("result-deliver", 1)["error"], "outbox_not_head")
        self.assertEqual(len(self.received), before)
        self.drop_after = True
        first = self.command("claim-deliver", 1)
        self.assertEqual(first["state"], "unresolved")
        original = self.received[-1]
        accepted_state = self.stored()
        retried = self.command("claim-deliver")
        self.assertEqual(self.received[-1], original)
        self.assertEqual(self.stored(), accepted_state)
        self.assertEqual(hashlib.sha256(original).hexdigest(), retried["envelope_sha256"])
        before = len(self.received)
        self.assertEqual(self.command("claim-deliver"), retried)
        self.assertEqual(len(self.received), before)
        ack = retried["response"]["data"]["ack"]
        self.values["FENCE"] = str(ack["fence"])
        work = self.command("read-work")["data"]["work"]
        self.assertEqual((work["state"], work["attempt_grant_id"], work["fence"]), ("claimed", self.child["id"], ack["fence"]))
        self.grant_check()
        posted = self.command("result-deliver")
        self.values["RESULT_ID"] = posted["response"]["receipt"]["id"]
        self.assertEqual(self.command("read-work")["data"]["work"]["state"], "claimed")
        self.grant_check()
        self.enqueue("submit")
        self.assertEqual(self.command("submit-deliver")["response"]["data"]["ack"]["state"], "submitted")
        self.assertEqual(self.command("read-work")["data"]["work"]["state"], "submitted")
        self.command("read-brief")
        result = self.command("read-result")["messages"][0]
        self.assertEqual((result["text"], result["reply_to"], result["author"]), (EVIDENCE, self.values["WORK_ID"], self.child["id"]))
        self.enqueue("accept")
        self.assertEqual(self.command("accept-deliver")["items"][0]["state"], "acknowledged")
        self.assertEqual(self.command("read-work")["data"]["work"]["state"], "accepted")
        self.assertEqual([row[2] for row in self.stored()[0]], ["work.create", "work.claim", "work.submit", "work.accept"])
        self.assertEqual(self.observed_gets[0], "/capabilities")

    def invalidated_retry(self, invalidation):
        self.capabilities_check()
        self.seed_request()
        self.grant_check()
        self.enqueue("claim")
        action = "claim-deliver"
        if invalidation == "revoke":
            self.command("claim-deliver")
            self.enqueue("result")
            action = "result-deliver"
        self.drop_before = True
        first = self.command(action, 1)
        self.assertEqual(first["state"], "unresolved")
        original = self.received[-1]
        if invalidation == "revoke":
            self.f.owner.command("delegation.revoke", target=self.child["id"],
                        data=json.dumps({"schema": 1, "generation": self.generation}))
            self.grant_check("revoked")
            # Job state alone is not a current delegation authorization check.
            self.assertEqual(self.command("read-work")["data"]["work"]["state"], "claimed")
        else:
            self.f.stop()
            subprocess.run([str(Path(BINARY).resolve()), "recover-generation", "--offline-confirmed"],
                env=self.f.env, check=True, capture_output=True, timeout=10)
            self.f.start()
            self.grant_check("epoch_disabled")
        before = self.stored()
        denied = self.command(action, 1)
        self.assertEqual((denied["state"], denied["last_error"]), ("blocked", "delegation_inactive"))
        self.assertEqual(self.received[-1], original)
        self.assertEqual(self.stored(), before)
        sent = len(self.received)
        self.assertEqual(self.command(action, 1), denied)
        self.assertEqual(len(self.received), sent)

    def test_prepared_retry_does_not_repair_revoked_authority(self):
        self.invalidated_retry("revoke")

    def test_prepared_retry_does_not_repair_recovery_generation(self):
        self.invalidated_retry("recovery")


if __name__ == "__main__":
    unittest.main()
