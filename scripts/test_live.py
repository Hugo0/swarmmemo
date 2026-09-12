"""Independent HTTP security regressions; run with SWARMMEMO_TEST_BINARY."""
import http.client
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.parse

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "clients/python"))
import swarmmemo as client


@unittest.skipUnless(os.environ.get("SWARMMEMO_TEST_BINARY"), "set SWARMMEMO_TEST_BINARY for security integration")
class HTTPSecurityTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory()
        work = Path(cls.temp.name)
        with socket.socket() as s:
            s.bind(("127.0.0.1", 0)); cls.port = s.getsockname()[1]
        cls.origin = "http://127.0.0.1:" + str(cls.port)
        cls.process = subprocess.Popen([os.environ["SWARMMEMO_TEST_BINARY"], "serve"],
            env={**os.environ, "DATA_DIR": str(work / "data"), "LISTEN_ADDR": "127.0.0.1:" + str(cls.port),
                 "ALLOW_INSECURE_LOCAL": "true", "ARCHIVE_DELAY_SECONDS": "1"},
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for _ in range(100):
            try:
                connection = http.client.HTTPConnection("127.0.0.1", cls.port, timeout=1)
                connection.request("GET", "/health")
                response = connection.getresponse(); response.read(); connection.close()
                if response.status == 200: break
            except OSError: time.sleep(0.05)
        else: raise RuntimeError("test server did not start")
        private, _, _ = client.crypto()
        cls.alice = client.Client(cls.origin, private.generate())
        cls.alice.command("room.create", room="hidden-room", visibility="private")
        cls.secret = "UNIQUE_PRIVATE_SENTINEL_74d939"
        cls.memo = cls.alice.post("hidden-room", "main", cls.secret)["receipt"]["id"]
        cls.public = client.Client(cls.origin)
        cls.public.post("lobby", "main", "Visible text <script>alert('x')</script>")

    @classmethod
    def tearDownClass(cls):
        cls.process.terminate()
        try: cls.process.wait(timeout=5)
        except subprocess.TimeoutExpired: cls.process.kill(); cls.process.wait()
        cls.temp.cleanup()

    def request(self, method, path, body=None, headers=None):
        connection = http.client.HTTPConnection("127.0.0.1", self.port, timeout=5)
        connection.request(method, path, body=body, headers=headers or {})
        response = connection.getresponse()
        content = response.read(); result = response.status, dict(response.getheaders()), content
        connection.close(); return result

    def test_post_cannot_silently_ignore_private_visibility(self):
        body = json.dumps({"operation": "post", "room": "new-private-intent", "visibility": "private", "text": "intended-private"})
        status, _, _ = self.request("POST", "/v1/command", body, {"Content-Type": "application/json"})
        self.assertGreaterEqual(status, 400, "private intent was silently published in a public room")

    def test_malformed_query_does_not_default_destination(self):
        status, _, _ = self.request("PUT", "/v1/events/malformed-query?room=hidden-room%ZZ&page=main",
                                     "intended-private-malformed-destination", {"Content-Type": "text/plain"})
        self.assertEqual(status, 400, "malformed room was discarded and defaulted to public lobby")

    def test_case_alias_duplicate_fields_rejected(self):
        body = b'{"operation":"post","room":"lobby","text":"first","Text":"second"}'
        status, _, _ = self.request("POST", "/v1/command", body, {"Content-Type": "application/json"})
        self.assertEqual(status, 400, "case-insensitive alias bypassed duplicate-field rejection")

    def test_exact_duplicate_json_rejected(self):
        body = b'{"operation":"post","text":"first","text":"second"}'
        status, _, _ = self.request("POST", "/v1/command", body, {"Content-Type": "application/json"})
        self.assertEqual(status, 400)

    def test_private_text_absent_on_public_surfaces(self):
        for path in ("/api/messages?cursor=start", "/api/messages?room=hidden-room", "/api/rooms", "/api/stats", "/api/agents",
                     "/api/room/hidden-room", "/e/" + self.memo, "/r/hidden-room", "/v1/export", "/", "/rooms"):
            with self.subTest(path=path):
                _, headers, body = self.request("GET", path, headers={"Accept": "text/html" if path in ("/", "/rooms") else "application/json"})
                self.assertNotIn(self.secret.encode(), body)
                self.assertEqual(headers.get("Cache-Control"), "no-store")
        for accept in ("text/html", "application/json", "text/plain", "application/json, text/html"):
            with self.subTest(accept=accept):
                status, _, body = self.request("GET", "/e/" + self.memo, headers={"Accept": accept})
                self.assertEqual(status, 404); self.assertNotIn(self.secret.encode(), body)

    def test_private_sse_is_denied_before_streaming(self):
        status, _, body = self.request("GET", "/api/stream?room=hidden-room")
        self.assertEqual(status, 404); self.assertNotIn(self.secret.encode(), body)

    def test_sse_duplicate_room_is_rejected(self):
        # The first room is inaccessible, so a non-strict parser returns 404 immediately.
        status, _, _ = self.request("GET", "/api/stream?room=hidden-room&room=lobby")
        self.assertEqual(status, 400)

    def test_html_escapes_messages_and_head_does_not_write(self):
        _, headers, body = self.request("GET", "/r/lobby", headers={"Accept": "text/html"})
        self.assertIn(b"&lt;script&gt;", body); self.assertNotIn(b"<script>alert", body)
        self.assertIn("script-src 'self'", headers.get("Content-Security-Policy", ""))
        before = self.public.command("stats")["stats"]
        for method in ("HEAD", "OPTIONS"):
            self.request(method, "/w/lobby/main?text=must-not-post")
            self.request(method, "/c64/" + client.b64(b'{"operation":"post","text":"must-not-post"}'))
        self.assertEqual(before, self.public.command("stats")["stats"])

    def test_noncanonical_base64_newline_rejected(self):
        status, _, _ = self.request("GET", "/w64/lobby/main/aGVs%0AbG8")
        self.assertEqual(status, 400)

    def test_mcp_cannot_override_text_with_case_alias(self):
        before = self.public.command("stats")["stats"]
        body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {
            "name": "post_message", "arguments": {"room": "lobby", "text": "first", "Text": "second"}}})
        status, _, response = self.request("POST", "/mcp", body, {"Content-Type": "application/json",
            "Accept": "application/json, text/event-stream", "MCP-Protocol-Version": "2025-06-18"})
        self.assertEqual(status, 200)
        result = json.loads(response)
        self.assertTrue("error" in result or result.get("result", {}).get("isError"), result)
        self.assertEqual(before, self.public.command("stats")["stats"])

    def test_private_activity_does_not_change_public_counts_or_sequence(self):
        first = self.public.post("lobby", "main", "public-before-private-sequence")["receipt"]["id"]
        first_event = self.public.command("message.get", message_id=first)["messages"][0]
        before = self.public.command("stats")["stats"]
        for i in range(3): self.alice.post("hidden-room", "main", "private-counter-probe-" + str(i))
        self.assertEqual(before, self.public.command("stats")["stats"])
        second = self.public.post("lobby", "main", "public-after-private-sequence")["receipt"]["id"]
        second_event = self.public.command("message.get", message_id=second)["messages"][0]
        self.assertEqual(second_event["sequence"], first_event["sequence"] + 1)


if __name__ == "__main__": unittest.main()
