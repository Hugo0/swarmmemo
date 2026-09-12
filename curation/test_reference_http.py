"""Synthetic guarded publisher -> real Go Reader/HTTP integration; no remote fetch.

Run with PYTHONDONTWRITEBYTECODE=1 SWARMMEMO_TEST_BINARY=/absolute/candidate
python3 -B -m unittest discover -s curation -p test_reference_http.py.
The binary must include the reference reader. Only fresh owned temporary paths
and an ephemeral 127.0.0.1 listener are used; no native messages are created.
"""
import copy
from datetime import datetime, timezone
import http.client
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch
from urllib.parse import urlencode

import publish_references as pub
import sync_sources as sync
from test_publish_references import item, policy_value, response


def utc(value):
    return datetime.fromtimestamp(value, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


@unittest.skipUnless(os.environ.get("SWARMMEMO_TEST_BINARY"),
                     "set SWARMMEMO_TEST_BINARY for real reference HTTP integration")
class ReferenceHTTPTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="swarmmemo-reference-http-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.catalog = self.root / "catalog.sqlite"
        self.registry = self.root / "registry.json"
        self.suppression = self.root / "suppression.json"
        self.snapshot = self.root / "snapshot.json"
        self.policy = policy_value()
        self.now = int(time.time())
        for grant in self.policy["sources"][0]["permissions"].values():
            grant.update(reviewed_at=utc(self.now - 3 * 86400), expires_at=utc(self.now + 86400))
        self.write_policy()
        self.write(self.suppression, pub.canonical({"version": 1, "ids": []}))
        self.items = [item("one", "BODY-ONLY-CANARY <script>window.referenceXSS=1</script> 雪"),
                      item("two", "Second external excerpt"), item("three", "Third external excerpt")]
        self.items[0]["title"] = "Synthetic alpha 雪 <script>not executable</script>"
        self.items[1]["title"] = "Synthetic beta"
        self.items[2]["title"] = "Synthetic gamma"
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            self.port = listener.getsockname()[1]
        self.origin = "http://127.0.0.1:" + str(self.port)
        # Do not inherit credentials, unrelated reference paths, proxy settings,
        # admin-token paths, or user production configuration into the server.
        environment = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"),
                       "DATA_DIR": str(self.root / "board"), "LISTEN_ADDR": "127.0.0.1:" + str(self.port),
                       "PUBLIC_URL": self.origin, "SERVICE_ID": "reference-fixture.invalid",
                       "ALLOW_INSECURE_LOCAL": "true", "ARCHIVE_DELAY_SECONDS": "0",
                       "REFERENCE_REGISTRY_PATH": str(self.registry),
                       "REFERENCE_SUPPRESSION_PATH": str(self.suppression),
                       "REFERENCE_SNAPSHOT_PATH": str(self.snapshot),
                       "REFERENCE_OWNER_UID": str(os.getuid()), "PYTHONDONTWRITEBYTECODE": "1"}
        binary = str(Path(os.environ["SWARMMEMO_TEST_BINARY"]).resolve(strict=True))
        self.process = subprocess.Popen([binary, "serve"], env=environment,
                                        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(self.stop)
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                self.fail("isolated Go candidate exited before readiness")
            try:
                if self.get("/health")[0] == 200:
                    break
            except OSError:
                time.sleep(0.025)
        else:
            self.fail("isolated Go candidate did not become ready")
        self.native_before = self.json_get("/api/stats")["stats"]

    def stop(self):
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)

    def write(self, path, raw):
        pub._atomic(path, raw, mode=0o600)

    def write_policy(self):
        self.write(self.registry, pub.canonical(self.policy))

    def publish(self, *, now=None, collect=True, items=None, reply=None, failure=None):
        observed = int(time.time()) if now is None else now
        fixture_reply = reply if reply is not None else response(self.items if items is None else items)

        def fixture_fetch(*_):
            if failure is not None:
                raise failure
            return copy.deepcopy(fixture_reply)

        # The reviewed supervised child inherits these test-only patches. No
        # URL opener, source DNS, browser or HTTP fixture feed is involved.
        with patch.object(sync, "fetch_feed", fixture_fetch), patch.object(pub, "_now", lambda: observed), \
                patch.object(sync.time, "time", lambda: observed):
            return pub.publish(self.catalog, self.registry, self.suppression, self.snapshot,
                               collect=collect, total_timeout=10)

    def get(self, path, *, method="GET", headers=None):
        self.assertTrue(path.startswith("/") and not path.startswith("//"))
        connection = http.client.HTTPConnection("127.0.0.1", self.port, timeout=3)
        try:
            connection.request(method, path, headers=headers or {})
            result = connection.getresponse()
            raw = result.read((1 << 20) + 1)
            self.assertLessEqual(len(raw), 1 << 20)
            return result.status, dict(result.getheaders()), raw
        finally:
            connection.close()

    def json_get(self, path, status=200):
        actual, headers, raw = self.get(path)
        self.assertEqual(actual, status, raw[:256])
        return json.loads(raw)

    def assert_unavailable(self, paths):
        for path in paths:
            for method in ("GET", "HEAD"):
                status, headers, body = self.get(path, method=method,
                    headers={"If-None-Match": '"stale"', "If-Modified-Since": "Tue, 01 Jan 2030 00:00:00 GMT"})
                self.assertEqual(status, 503)
                self.assertEqual(headers.get("Cache-Control"), "no-store")
                for canary in (b"BODY-ONLY-CANARY", b"Synthetic alpha", b"DO-NOT-REFLECT", b"External fixture author"):
                    self.assertNotIn(canary, body)
                if method == "HEAD":
                    self.assertEqual(body, b"")

    def assert_native_unchanged(self):
        self.assertEqual(self.json_get("/api/stats")["stats"], self.native_before)
        self.assertFalse(self.json_get("/api/messages").get("messages"))
        self.assertFalse(self.json_get("/api/works")["data"].get("works"))
        self.assertFalse(self.json_get("/api/agents").get("agents"))
        status, _, exported = self.get("/v1/export")
        self.assertEqual(status, 200)
        self.assertEqual(exported, b"")

    def test_ready_list_detail_literal_search_pagination_and_boundaries(self):
        self.publish()
        rows = self.json_get("/api/references")["references"]
        self.assertEqual(len(rows), 3)
        self.assertEqual([row["id"] for row in rows], sorted(row["id"] for row in rows))
        for row in rows:
            self.assertFalse(row["native_identity"] or row["claimable_job"] or row["hugging_face_eligible"])
            self.assertTrue(row["untrusted_content"])
            detail = self.json_get("/api/references/" + row["id"])
            self.assertEqual(detail["reference"], row)
        first = self.json_get("/api/references?limit=1")
        second = self.json_get("/api/references?" + urlencode({"limit": 1, "cursor": first["next_cursor"]}))
        self.assertNotEqual(first["references"][0]["id"], second["references"][0]["id"])
        for query, expected in (("body-only-canary", 1), ("雪", 1), ("alpha OR beta", 0)):
            self.assertEqual(len(self.json_get("/api/references?" + urlencode({"q": query}))["references"]), expected)
        self.assertEqual(self.json_get("/api/references?source=absent")["references"], [])
        status, headers, html = self.get("/references")
        self.assertEqual(status, 200)
        self.assertEqual(headers.get("Content-Signal"), "search=yes,ai-train=no,use=reference")
        self.assertEqual(headers.get("Cache-Control"), "no-store")
        self.assertIn(b"&lt;script&gt;", html)
        self.assertNotIn(b"<script>window.referenceXSS", html)
        self.assertNotIn(b"DO-NOT-REFLECT", html)
        self.assertIn(b"not native posts or available jobs", html)
        for path in ("/references/" + rows[0]["id"], "/api/references/" + rows[0]["id"]):
            self.assertEqual(self.get(path, method="HEAD")[0::2], (200, b""))
        for query in ("q=a&q=b", "operation=post", "limit=51", "limit=01"):
            self.assertEqual(self.get("/api/references?" + query)[0], 400)
        self.assert_native_unchanged()

    def test_distributed_disabled_registry_and_optional_name_interoperate(self):
        self.write(self.registry, Path(sync.__file__).with_name("sync-sources.example.json").read_bytes())
        with patch.object(sync, "fetch_feed", side_effect=AssertionError("disabled source fetched")):
            pub.publish(self.catalog, self.registry, self.suppression, self.snapshot, collect=True)
        self.assertEqual(self.json_get("/api/references")["references"], [])
        del self.policy["sources"][0]["name"]
        self.policy["sources"][0]["terms_url"] = None
        self.write_policy()
        self.publish()
        self.assertEqual(self.json_get("/api/references")["sources"][0]["name"], "")
        self.assertEqual(self.get("/references")[0], 200)
        self.assert_native_unchanged()

    def test_policy_change_revokes_old_snapshot_and_metadata_only_search(self):
        self.publish()
        rid = pub.reference_id("fixture", "one")
        paths = ("/references", "/api/references?q=BODY-ONLY-CANARY", "/references/" + rid, "/api/references/" + rid)
        self.policy["sources"][0]["enabled"] = False
        self.write_policy()
        self.assert_unavailable(paths)
        self.publish()
        self.assertEqual(self.json_get("/api/references")["references"], [])
        self.assertEqual(self.get("/api/references/" + rid)[0], 404)
        self.policy["sources"][0]["enabled"] = True
        self.policy["sources"][0]["permissions"]["full_text_storage"]["approved"] = False
        self.write_policy()
        self.publish()
        self.assertEqual(self.json_get("/api/references?q=BODY-ONLY-CANARY")["references"], [])
        row = self.json_get("/api/references?q=alpha")["references"][0]
        self.assertEqual((row["excerpt"], row["excerpt_available"], row["excerpt_truncated"]), ("", False, False))
        self.assert_native_unchanged()

    def test_suppression_blocks_then_recovers_without_native_or_body_leak(self):
        self.publish()
        rid = pub.reference_id("fixture", "one")
        self.write(self.suppression, pub.canonical({"version": 1, "ids": [rid]}))
        self.assert_unavailable(("/references/" + rid, "/api/references/" + rid, "/api/references"))
        with self.assertRaises(pub.PublicationError):
            self.publish(collect=False)
        self.publish()
        self.assertEqual(len(self.json_get("/api/references")["references"]), 2)
        for path in ("/references/" + rid, "/api/references/" + rid):
            status, _, body = self.get(path)
            self.assertEqual(status, 404)
            self.assertNotIn(b"BODY-ONLY-CANARY", body)
            self.assertNotIn(b"Synthetic alpha", body)
        self.assert_native_unchanged()

    def test_failure_stays_blocked_and_ready_revision_resets_cursor(self):
        self.publish()
        old = self.json_get("/api/references?limit=1")
        with self.assertRaisesRegex(pub.PublicationError, "source_failed"):
            self.publish(failure=TimeoutError("UNTRUSTED-FAILURE-CANARY"))
        self.assert_unavailable(("/references", "/api/references"))
        with self.assertRaisesRegex(pub.PublicationError, "publication_blocked"):
            self.publish(collect=False)
        self.items[0]["content_text"] += " revised"
        self.publish()
        changed = self.json_get("/api/references")
        self.assertNotEqual(old["snapshot_sha256"], changed["snapshot_sha256"])
        self.json_get("/api/references?" + urlencode({"limit": 1, "cursor": old["next_cursor"]}), status=409)
        self.assert_native_unchanged()

    def test_actual_permission_expiry_hides_ready_bytes_without_file_change(self):
        expires = int(time.time()) + 4
        self.policy["sources"][0]["permissions"]["public_archive"]["expires_at"] = utc(expires)
        self.write_policy()
        self.publish()
        self.assertEqual(self.json_get("/api/references")["valid_until"], expires)
        raw = self.snapshot.read_bytes()
        time.sleep(max(0, expires - time.time()) + 0.05)
        self.assertEqual(self.snapshot.read_bytes(), raw)
        self.assert_unavailable(("/references", "/api/references", "/api/references?q=BODY-ONLY-CANARY"))
        self.assert_native_unchanged()

    def test_historical_cuttle_fixture_retains_raw_date_and_dual_authorship(self):
        self.policy["sources"][0].update(adapter=sync.CUTTLE_ADAPTER, feed_url=sync.CUTTLE_FEED)
        self.write_policy()
        row = {"slug": "synthetic-worklog", "url": sync.CUTTLE_ORIGIN + "/synthetic-worklog/",
               "title": "Synthetic co-created worklog 雪", "excerpt": "Synthetic source excerpt, not a real Cuttle post.",
               "published_at": "2026-09-04 12:00:00", "series": "weekly-digests",
               "6529_learning_path": None, "agents_learning_path": None}
        observed = int(time.time()) - 90000
        self.publish(now=observed, reply={"status": 200, "body": pub.canonical([row])})
        self.publish(reply={"status": 304})
        current = self.json_get("/api/references")
        ref = current["references"][0]
        self.assertEqual(ref["last_observed_at"], observed)
        self.assertGreater(current["sources"][0]["last_successful_at"], observed)
        self.assertEqual(ref["source_published_at"], row["published_at"])
        self.assertFalse(ref["source_publication_timezone_known"])
        self.assertEqual(len(ref["authors"]), 2)
        self.assertEqual(ref["excerpt"], row["excerpt"])
        html = self.get("/references")[2]
        for label in (b"Historical reference", b"timezone not supplied", b"Last seen in source index", b"Source last checked"):
            self.assertIn(label, html)
        self.assert_native_unchanged()

    @unittest.skipUnless(os.environ.get("SWARMMEMO_REFERENCE_BROWSER") == "1",
                         "set SWARMMEMO_REFERENCE_BROWSER=1 with Playwright for browser fixtures")
    def test_optional_browser_reference_view(self):
        self.items[0]["title"] += "LongTitle" * 40
        self.items[0]["content_text"] += "L" * 600
        self.publish()
        root = Path(__file__).resolve().parent.parent
        environment = {key: value for key, value in os.environ.items()
                       if key in ("PATH", "PLAYWRIGHT_MODULE", "CHROMIUM_PATH", "PLAYWRIGHT_NO_SANDBOX")}
        environment["SWARMMEMO_TEST_URL"] = self.origin
        result = subprocess.run(["node", str(root / "internal/web/references_test.cjs")],
                                cwd=root, env=environment, capture_output=True, timeout=45)
        self.assertEqual(result.returncode, 0, (result.stdout + result.stderr).decode("utf-8", "replace")[:4096])
        self.assert_native_unchanged()


if __name__ == "__main__":
    unittest.main()
