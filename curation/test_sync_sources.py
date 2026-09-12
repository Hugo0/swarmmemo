import contextlib
from copy import deepcopy
import io
import json
import os
from pathlib import Path
import signal
import sqlite3
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

import sync_sources as sync

NOW = sync.timestamp("2026-09-05T00:00:00Z")


def registry_value():
    grant = {"approved": True, "evidence_url": "https://example.org/permission/swarmmemo",
             "reviewed_at": "2026-09-01T00:00:00Z", "expires_at": "2026-10-01T00:00:00Z",
             "scope": "Fixture operator explicitly permits this named activity"}
    return {"version": 1, "sources": [{"id": "forum", "name": "Fixture forum", "adapter": "jsonfeed-1.1",
        "feed_url": "https://example.org/feed.json", "enabled": True,
        "permissions": {right: deepcopy(grant) for right in sync.RIGHTS}}]}


def item(external_id="item-1", text="searchable collaboration", **fields):
    return {"id": external_id, "title": "A forum item", "url": "https://example.org/item/" + external_id,
            "content_text": text, "authors": [{"name": "ExternalAgent", "url": "https://example.org/agent/a"}],
            "date_published": "2026-08-31T18:00:00Z", **fields}


def response(items, status=200, etag='"fixture-v1"'):
    return {"status": status, "etag": etag, "last_modified": "Sat, 05 Sep 2026 00:00:00 GMT",
            "body": sync.encoded({"version": sync.FEED_VERSION, "title": "Fixture", "items": items}) if status == 200 else b""}


class CatalogTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.path = self.root / "catalog.sqlite"
        self.registry_path = self.root / "registry.json"
        self.registry = self.load(registry_value())

    def tearDown(self): self.temporary.cleanup()

    def load(self, value):
        self.registry_path.write_text(json.dumps(value))
        return sync.load_registry(self.registry_path)

    def run_sync(self, records, registry=None, now=NOW):
        result = sync._synchronize(registry or self.registry, self.path, now=now,
                                  fetcher=lambda source, validators: response(records))
        self.assertFalse(any(row["status"] == "failed" for row in result), result)
        return result

    def query(self, query, registry=None, now=NOW):
        return sync.search_catalog(registry or self.registry, self.path, query, now=now)

    def test_dry_run_is_network_free_and_creates_no_catalog(self):
        with patch.object(sync, "synchronize", side_effect=AssertionError("must not sync")), contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(sync.main(["--registry", str(self.registry_path), "--catalog", str(self.path)]), 0)
        self.assertFalse(self.path.exists())
        self.assertEqual(json.loads(output.getvalue())["network_requests"], 0)

    def test_dedup_revision_provenance_and_conditional_requests(self):
        calls = []
        replies = [response([item()]), response([], 304), response([item(text="revised collaboration")], etag='"fixture-v2"')]
        def fetch(source, validators):
            calls.append(deepcopy(validators)); return replies.pop(0)
        for n in range(3): sync._synchronize(self.registry, self.path, now=NOW+n, fetcher=fetch)
        self.assertEqual(calls[1]["etag"], '"fixture-v1"')
        self.run_sync([item(text="revised collaboration")], now=NOW+3)
        with contextlib.closing(sqlite3.connect(self.path)) as db:
            self.assertEqual(db.execute("SELECT count(*) FROM items").fetchone()[0], 1)
            self.assertEqual(db.execute("SELECT count(*) FROM revisions").fetchone()[0], 2)
        results = self.query("revised")
        self.assertEqual(len(results), 1)
        self.assertEqual(results[0]["source_published_at"], "2026-08-31T18:00:00Z")
        self.assertEqual(results[0]["first_observed_at"], NOW)
        self.assertEqual(results[0]["authors"][0]["name"], "ExternalAgent")
        self.assertNotIn("board_identity", results[0])

    def test_explicit_tombstone_purges_history_and_prevents_resurrection(self):
        self.run_sync([item(text="first-secret-body")])
        self.run_sync([item(text="second-secret-body")], now=NOW+1)
        self.run_sync([{ "id": "item-1", "_swarmmemo": {"deleted": True, "revision": "delete-v3"}}], now=NOW+2)
        with contextlib.closing(sqlite3.connect(self.path)) as db:
            self.assertEqual(db.execute("SELECT body,body_text,title,deleted FROM items").fetchone(), (None, "", "", 1))
            self.assertEqual(db.execute("SELECT count(*) FROM revisions WHERE body IS NOT NULL OR body_text<>'' OR title<>''").fetchone()[0], 0)
        self.assertEqual(self.query("secret"), [])
        result = self.run_sync([item(text="newer-or-stale-body", date_modified="2026-09-30T00:00:00Z")], now=NOW+3)
        self.assertEqual(result[0]["resurrections_blocked"], 1)
        self.assertEqual(self.query("newer"), [])

    def test_missing_items_are_not_deletions(self):
        self.run_sync([item()])
        self.run_sync([])
        self.assertEqual(len(self.query("collaboration")), 1)

    def test_no_fulltext_permission_stores_only_metadata(self):
        registry = deepcopy(self.registry)
        registry["sources"][0]["permissions"]["full_text_storage"] = {"approved": False}
        self.run_sync([item(text="not-permitted-secret")], registry)
        with contextlib.closing(sqlite3.connect(self.path)) as db:
            self.assertEqual(db.execute("SELECT body,body_text FROM items").fetchone(), (None, ""))
            self.assertEqual(db.execute("SELECT body FROM revisions").fetchone(), (None,))
        self.assertEqual(self.query("not-permitted", registry), [])
        self.assertEqual(len(self.query("forum", registry)), 1)

    def test_public_search_and_hf_rights_are_separate(self):
        self.run_sync([item()])
        registry = deepcopy(self.registry)
        registry["sources"][0]["permissions"]["public_archive"] = {"approved": False}
        self.assertEqual(self.query("collaboration", registry), [])
        registry = deepcopy(self.registry)
        registry["sources"][0]["permissions"]["hugging_face"] = {"approved": False}
        self.assertFalse(self.query("collaboration", registry)[0]["hugging_face_eligible"])

    def test_disabled_removed_and_expired_sources_cannot_expose_old_bodies(self):
        self.run_sync([item()])
        for mutation in ("disabled", "removed", "expired"):
            registry = deepcopy(self.registry)
            if mutation == "disabled": registry["sources"][0]["enabled"] = False
            elif mutation == "removed": registry["sources"] = []
            else: registry["sources"][0]["permissions"]["automated_collection"]["expires_at"] = "2026-09-04T00:00:00Z"
            with self.subTest(mutation=mutation):
                self.assertEqual(self.query("collaboration", registry), [])
                sync._synchronize(registry, self.path, now=NOW, fetcher=lambda *_: self.fail("revoked source fetched"))
                with contextlib.closing(sqlite3.connect(self.path)) as db:
                    self.assertEqual(db.execute("SELECT count(*) FROM items WHERE body IS NOT NULL").fetchone()[0], 0)
                    self.assertEqual(db.execute("SELECT count(*) FROM revisions WHERE body IS NOT NULL").fetchone()[0], 0)

    def test_storage_expiry_does_not_leak_body_search_matches(self):
        self.run_sync([item(text="privatecanaryuniquetoken")])
        registry = deepcopy(self.registry)
        registry["sources"][0]["permissions"]["full_text_storage"]["expires_at"] = "2026-09-04T00:00:00Z"
        self.assertEqual(self.query("privatecanaryuniquetoken", registry), [])
        result = self.query("forum", registry)
        self.assertEqual(len(result), 1); self.assertIsNone(result[0]["body_text"])

    def test_permissions_expiring_during_fetch_are_rechecked(self):
        for right in ("automated_collection", "full_text_storage"):
            with self.subTest(right=right):
                self.run_sync([item()])
                registry = deepcopy(self.registry)
                registry["sources"][0]["permissions"][right]["expires_at"] = "2026-09-05T00:00:01Z"
                with patch.object(sync.time, "time", return_value=NOW) as clock:
                    def fetch(*_):
                        clock.return_value = NOW+2
                        return response([], 304)
                    result = sync._synchronize(registry, self.path, fetcher=fetch)
                expected = "permission_blocked" if right == "automated_collection" else "not_modified"
                self.assertEqual(result[0]["status"], expected)
                with contextlib.closing(sqlite3.connect(self.path)) as db:
                    self.assertEqual(db.execute("SELECT count(*) FROM items WHERE body IS NOT NULL").fetchone()[0], 0)
                    self.assertEqual(db.execute("SELECT count(*) FROM revisions WHERE body IS NOT NULL").fetchone()[0], 0)

    def test_failed_fetch_still_purges_expired_current_and_revision_bodies(self):
        for right in ("automated_collection", "full_text_storage"):
            with self.subTest(right=right):
                self.run_sync([item()])
                registry = deepcopy(self.registry)
                registry["sources"][0]["permissions"][right]["expires_at"] = "2026-09-05T00:00:01Z"
                with patch.object(sync.time, "time", return_value=NOW) as clock:
                    def fetch(*_):
                        clock.return_value = NOW+2
                        raise TimeoutError("remote failure with sensitive details")
                    result = sync._synchronize(registry, self.path, fetcher=fetch)
                self.assertEqual(result[0]["error"], "TimeoutError")
                with contextlib.closing(sqlite3.connect(self.path)) as db:
                    self.assertEqual(db.execute("SELECT count(*) FROM items WHERE body IS NOT NULL OR body_text<>''").fetchone()[0], 0)
                    self.assertEqual(db.execute("SELECT count(*) FROM revisions WHERE body IS NOT NULL OR body_text<>''").fetchone()[0], 0)

    def test_unrelated_source_rebind_cannot_roll_back_retention_purge(self):
        registry = deepcopy(self.registry)
        second = deepcopy(registry["sources"][0])
        second.update(id="second", feed_url="https://other.example/feed.json")
        registry["sources"].append(second)
        self.run_sync([item()], registry)
        registry["sources"][0]["enabled"] = False
        registry["sources"][1]["feed_url"] = "https://different.example/feed.json"
        with self.assertRaisesRegex(sync.SyncError, "source_identity_changed_use_new_id"):
            sync._synchronize(registry, self.path, now=NOW, fetcher=lambda *_: self.fail("must not fetch"))
        with contextlib.closing(sqlite3.connect(self.path)) as db:
            self.assertEqual(db.execute("SELECT count(*) FROM items WHERE source_id='forum' AND (body IS NOT NULL OR body_text<>'')").fetchone()[0], 0)
            self.assertEqual(db.execute("SELECT count(*) FROM revisions r JOIN items i ON i.rowid=r.item_id WHERE i.source_id='forum' AND (r.body IS NOT NULL OR r.body_text<>'')").fetchone()[0], 0)
            self.assertEqual(db.execute("SELECT count(*) FROM items WHERE source_id='second' AND body IS NOT NULL").fetchone()[0], 1)

    def test_expiry_during_other_source_fetch_purges_skipped_and_unselected(self):
        registry = deepcopy(self.registry)
        second = deepcopy(registry["sources"][0])
        second.update(id="second", feed_url="https://other.example/feed.json")
        registry["sources"].append(second)
        for selected in (None, "forum"):
            with self.subTest(selected=selected):
                self.run_sync([item()], registry)
                expiring = deepcopy(registry)
                expiring["sources"][1]["permissions"]["automated_collection"]["expires_at"] = "2026-09-05T00:00:01Z"
                with patch.object(sync.time, "time", return_value=NOW) as clock:
                    def fetch(source, _validators):
                        self.assertEqual(source["id"], "forum")
                        clock.return_value = NOW+2
                        return response([], 304)
                    sync._synchronize(expiring, self.path, selected=selected, fetcher=fetch)
                with contextlib.closing(sqlite3.connect(self.path)) as db:
                    self.assertEqual(db.execute("SELECT count(*) FROM items WHERE source_id='second' AND body IS NOT NULL").fetchone()[0], 0)
                    self.assertEqual(db.execute("SELECT count(*) FROM revisions r JOIN items i ON i.rowid=r.item_id WHERE i.source_id='second' AND r.body IS NOT NULL").fetchone()[0], 0)

    def test_registry_change_cannot_rebind_old_content(self):
        self.run_sync([item()])
        registry = deepcopy(self.registry); registry["sources"][0]["feed_url"] = "https://different.example/feed.json"
        with self.assertRaises(sync.SyncError): self.query("collaboration", registry)
        with self.assertRaises(sync.SyncError): sync._synchronize(registry, self.path, now=NOW, fetcher=lambda *_: self.fail("rebound"))

    def test_invalid_batch_rolls_back_validators_and_items(self):
        self.run_sync([item()])
        bad = response([item("valid-next"), item("duplicate"), item("duplicate")], etag='"bad"')
        result = sync._synchronize(self.registry, self.path, now=NOW, fetcher=lambda *_: bad)
        self.assertEqual(result[0]["status"], "failed")
        with contextlib.closing(sqlite3.connect(self.path)) as db:
            self.assertEqual(db.execute("SELECT count(*) FROM items").fetchone()[0], 1)
            self.assertEqual(db.execute("SELECT etag FROM sources").fetchone()[0], '"fixture-v1"')

    def test_item_byte_count_and_revision_caps(self):
        source = self.registry["sources"][0]
        source["limits"].update(total_items=1, revisions_per_item=2)
        for n in range(4): self.run_sync([item(text="version " + str(n))], now=NOW+n)
        with contextlib.closing(sqlite3.connect(self.path)) as db:
            self.assertEqual(db.execute("SELECT count(*) FROM revisions").fetchone()[0], 2)
        result = sync._synchronize(self.registry, self.path, now=NOW, fetcher=lambda *_: response([item("second")]))
        self.assertEqual(result[0]["error"], "source_item_capacity")
        source["limits"]["items_per_fetch"] = 1
        with self.assertRaises(sync.SyncError): sync.parse_feed(response([item(), item("two")])["body"], source, NOW)
        source["limits"]["item_bytes"] = 20
        with self.assertRaises(sync.SyncError): sync.parse_feed(response([item()])["body"], source, NOW)

    def test_html_is_inert_and_assets_are_not_fetched(self):
        record = item(); del record["content_text"]
        record["content_html"] = '<p>Useful finding</p><script>exfiltrate()</script><img src="http://127.0.0.1/secret">'
        record["attachments"] = [{"url": "http://169.254.169.254/latest/meta-data"}]
        self.run_sync([record])
        result = self.query("Useful")
        self.assertIn("Useful finding", result[0]["body_text"])
        self.assertNotIn("exfiltrate", result[0]["body_text"])

    def test_permission_evidence_and_adapter_version_are_required(self):
        value = registry_value(); del value["sources"][0]["permissions"]["automated_collection"]["evidence_url"]
        with self.assertRaises(sync.SyncError): self.load(value)
        value = registry_value(); value["sources"][0]["adapter"] = "html-crawler"
        with self.assertRaises(sync.SyncError): self.load(value)
        source = self.registry["sources"][0]
        raw = sync.encoded({"version": "https://jsonfeed.org/version/1", "title": "old", "items": []})
        with self.assertRaises(sync.SyncError): sync.parse_feed(raw, source, NOW)

    def test_moltbook_example_has_no_collection_authority(self):
        registry = sync.load_registry(Path(__file__).with_name("sync-sources.example.json"))
        source = next(s for s in registry["sources"] if s["id"] == "moltbook")
        self.assertFalse(source["enabled"]); self.assertEqual(source["status"], "permission_required")
        self.assertFalse(sync.collectable(source, NOW))
        sync._synchronize(registry, self.path, now=NOW, fetcher=lambda *_: self.fail("disabled example fetched"))


class NetworkBoundaryTests(unittest.TestCase):
    def test_real_http_response_rejects_truncated_declared_length(self):
        source = registry_value()["sources"][0]
        source["limits"] = deepcopy(sync.DEFAULT_LIMITS)
        body = response([item()])["body"]
        class Socket:
            def __init__(self, length):
                self.wire = (b"HTTP/1.1 200 OK\r\nContent-Type: application/feed+json\r\n"
                             + b"Content-Length: " + str(length).encode() + b"\r\nETag: \"incomplete\"\r\n\r\n" + body)
            def makefile(self, *_args, **_kwargs): return io.BytesIO(self.wire)
        class Connection:
            def __init__(self, *_args): pass
            def request(self, *_args, **_kwargs): pass
            def getresponse(self):
                self.reply = sync.http.client.HTTPResponse(Socket(length))
                self.reply.begin()
                return self.reply
            def close(self): self.reply.close()
        length = len(body) + 20
        with self.assertRaisesRegex(sync.SyncError, "truncated_feed"):
            sync.fetch_feed(source, {}, lambda _: ["1.1.1.1"], Connection)
        length = len(body)
        self.assertEqual(sync.fetch_feed(source, {}, lambda _: ["1.1.1.1"], Connection)["body"], body)

    def test_url_and_dns_reject_private_redirect_targets_and_credentials(self):
        for url in ("http://example.org/feed", "https://127.0.0.1/feed", "https://[::1]/feed", "https://user:pass@example.org/feed",
                    "https://example.org:8443/feed", "https://localhost/feed", "https://169.254.169.254/feed"):
            with self.subTest(url=url), self.assertRaises(sync.SyncError): sync.https_url(url)
        for addresses in (["127.0.0.1"], ["2606:4700:4700::1111", "::1"], ["169.254.169.254"], ["224.0.0.1"]):
            with self.subTest(addresses=addresses), self.assertRaises(sync.SyncError): sync.check_addresses(addresses)

    def test_pinned_fetch_conditional_and_redirect_compression_bounds(self):
        source = registry_value()["sources"][0]; source["limits"] = deepcopy(sync.DEFAULT_LIMITS)
        class Response:
            status = 200
            headers = {"Content-Type": "application/feed+json", "ETag": '"v2"'}
            body = b'{}'
            def getheader(self, key, default=None): return self.headers.get(key, default)
            def read1(self, size): result, self.body = self.body[:size], self.body[size:]; return result
        seen = []
        reply = Response()
        class Connection:
            def __init__(self, hostname, address, timeout): seen.append((hostname, address, timeout))
            def request(self, method, target, headers): seen.append((method, target, headers))
            def getresponse(self): return reply
            def close(self): pass
        resolver = lambda host: ["1.1.1.1"]
        result = sync.fetch_feed(source, {"etag": '"v1"'}, resolver, Connection)
        self.assertEqual(result["body"], b'{}'); self.assertEqual(seen[0], ("example.org", "1.1.1.1", 5))
        self.assertEqual(seen[1][2]["If-None-Match"], '"v1"')
        reply.status = 302; reply.headers["Location"] = "https://127.0.0.1/secret"
        with self.assertRaises(sync.SyncError): sync.fetch_feed(source, {}, resolver, Connection)
        reply.status = 200; reply.headers["Content-Encoding"] = "gzip"
        with self.assertRaises(sync.SyncError): sync.fetch_feed(source, {}, resolver, Connection)
        del reply.headers["Content-Encoding"]; reply.body = b'x' * 20; source["limits"]["feed_bytes"] = 10
        with self.assertRaises(sync.SyncError): sync.fetch_feed(source, {}, resolver, Connection)


class SupervisorTests(unittest.TestCase):
    """All work is injected local fixtures; no real DNS or public socket requests."""

    setUp, tearDown = CatalogTests.setUp, CatalogTests.tearDown
    load, run_sync, query = CatalogTests.load, CatalogTests.run_sync, CatalogTests.query

    def bounded(self, fetcher, registry=None):
        # Exercise exactly the supervisor used by the public API with a short
        # test-only budget. Production validation still requires 10–300 seconds.
        with patch.object(sync, "RETENTION_SECONDS", 0.5), patch.object(sync, "REAP_SECONDS", 0.1):
            return sync._synchronize_supervised(registry or self.registry, self.path, None, NOW, fetcher, 1.0)

    def test_public_api_validates_deadline_and_cli_routes_through_supervisor(self):
        for timeout in (True, 0, 9, 301, 10.5, "60"):
            with self.subTest(timeout=timeout), self.assertRaisesRegex(sync.SyncError, "invalid_total_timeout"):
                sync.synchronize(self.registry, self.path, total_timeout=timeout)
        self.assertFalse(self.path.exists())
        with patch.object(sync.threading, "active_count", return_value=2):
            with self.assertRaisesRegex(sync.SyncError, "worker_requires_single_thread"):
                sync.synchronize(self.registry, self.path)
        with patch.object(sync, "synchronize", return_value=[]) as supervisor, contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(sync.main(["--registry", str(self.registry_path), "--catalog", str(self.path), "--sync", "--total-timeout", "27"]), 0)
        self.assertEqual(supervisor.call_args.kwargs["total_timeout"], 27)
        with contextlib.redirect_stderr(io.StringIO()) as errors:
            self.assertEqual(sync.main(["--registry", str(self.registry_path), "--catalog", str(self.path), "--sync", "--total-timeout", "1"]), 1)
        self.assertEqual(json.loads(errors.getvalue()), {"error": "invalid_total_timeout"})

    def test_successful_supervision_persists_complete_batch_and_closes_lock(self):
        result = sync.synchronize(self.registry, self.path, now=NOW,
                                  fetcher=lambda *_: response([item()]), total_timeout=10)
        self.assertEqual(result[0]["changed"], 1)
        self.assertEqual(len(self.query("collaboration")), 1)
        result = self.bounded(lambda *_: response([], 304))
        self.assertEqual(result[0]["status"], "not_modified")
        self.assertEqual(os.stat(self.path).st_mode & 0o777, 0o600)

    def test_unsafe_sigchld_dispositions_rejected_before_fork(self):
        previous = signal.getsignal(signal.SIGCHLD)
        try:
            for handler in (signal.SIG_IGN, lambda *_: None):
                signal.signal(signal.SIGCHLD, handler)
                with patch.object(sync.os, "fork") as fork:
                    with self.assertRaisesRegex(sync.SyncError, "^worker_requires_default_sigchld$"):
                        sync.synchronize(self.registry, self.path, now=NOW, fetcher=lambda *_: response([]))
                    with self.assertRaisesRegex(sync.SyncError, "^worker_requires_default_sigchld$"):
                        sync._supervised_call(lambda: None, time.monotonic()+1, time.monotonic()+2)
                    fork.assert_not_called()
        finally: signal.signal(signal.SIGCHLD, previous)

    def test_lost_child_ownership_never_signals_a_pid(self):
        with patch.object(sync.os, "waitid", side_effect=ChildProcessError), patch.object(sync.os, "killpg") as group, patch.object(sync.os, "kill") as direct:
            with self.assertRaisesRegex(sync.SyncError, "^worker_ownership_lost$"):
                sync._kill_and_reap(123456, time.monotonic()+1)
            group.assert_not_called(); direct.assert_not_called()

    def test_exception_and_signal_during_parent_setup_cannot_orphan_child(self):
        parent = os.getpid(); real_fork, real_close = os.fork, os.close
        for cause in ("keyboard", "term"):
            owned = []; injected = False
            mask = signal.pthread_sigmask(signal.SIG_BLOCK, set())
            handlers = {sig: signal.getsignal(sig) for sig in (signal.SIGINT, signal.SIGTERM)}
            def fork():
                pid = real_fork()
                if pid > 0: owned.append(pid)
                return pid
            def close(fd):
                nonlocal injected
                real_close(fd)
                if os.getpid() == parent and not injected:
                    injected = True
                    self.assertTrue({signal.SIGINT, signal.SIGTERM} <= signal.pthread_sigmask(signal.SIG_BLOCK, set()))
                    if cause == "keyboard": raise KeyboardInterrupt()
                    os.kill(parent, signal.SIGTERM)
            with self.subTest(cause=cause), patch.object(sync.os, "fork", side_effect=fork), patch.object(sync.os, "close", side_effect=close):
                with self.assertRaises(KeyboardInterrupt if cause == "keyboard" else sync.SyncError):
                    sync._supervised_call(lambda: time.sleep(20), time.monotonic()+1, time.monotonic()+1.2)
            self.assertEqual(len(owned), 1)
            with self.assertRaises(ChildProcessError): os.waitpid(owned[0], os.WNOHANG)
            self.assertEqual(signal.pthread_sigmask(signal.SIG_BLOCK, set()), mask)
            self.assertEqual({sig: signal.getsignal(sig) for sig in handlers}, handlers)

    def test_repeated_termination_during_group_cleanup_waits_until_child_reaped(self):
        real_killpg = os.killpg; owned = []
        mask = signal.pthread_sigmask(signal.SIG_BLOCK, set())
        def killpg(pid, sig):
            owned.append(pid)
            self.assertTrue({signal.SIGINT, signal.SIGTERM} <= signal.pthread_sigmask(signal.SIG_BLOCK, set()))
            real_killpg(pid, sig)
            for _ in range(2):
                os.kill(os.getpid(), signal.SIGINT)
                os.kill(os.getpid(), signal.SIGTERM)
        with patch.object(sync.os, "killpg", side_effect=killpg):
            with self.assertRaisesRegex(sync.SyncError, "^sync_interrupted$"):
                sync._supervised_call(lambda: {"fixture": True}, time.monotonic()+1, time.monotonic()+1.2)
        self.assertEqual(len(owned), 1)
        with self.assertRaises(ChildProcessError): os.waitpid(owned[0], os.WNOHANG)
        self.assertEqual(signal.pthread_sigmask(signal.SIG_BLOCK, set()), mask)

    def test_worker_and_parent_restore_callers_signal_mask(self):
        previous = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGUSR1})
        expected = signal.pthread_sigmask(signal.SIG_BLOCK, set())
        try:
            actual = sync._supervised_call(lambda: sorted(map(int, signal.pthread_sigmask(signal.SIG_BLOCK, set()))),
                                           time.monotonic()+1, time.monotonic()+1.2)
            self.assertEqual(actual, sorted(map(int, expected)))
            self.assertEqual(signal.pthread_sigmask(signal.SIG_BLOCK, set()), expected)
        finally: signal.pthread_sigmask(signal.SIG_SETMASK, previous)

    def test_dns_headers_and_trickle_are_all_killable(self):
        for phase in ("dns", "headers", "framing"):
            with self.subTest(phase=phase):
                self.run_sync([item()])
                def fetch(source, validators):
                    def resolver(_host):
                        if phase == "dns": time.sleep(20)
                        return ["1.1.1.1"]
                    class Response:
                        status = 200
                        def getheader(self, name, default=None):
                            return "application/json" if name == "Content-Type" else default
                        def read1(self, _amount):
                            # Simulate never-ending chunk framing or a header
                            # parser receiving bytes frequently enough to keep
                            # an inactivity timeout from firing.
                            while True: time.sleep(0.01)
                    class Connection:
                        def __init__(self, *_): pass
                        def request(self, *_args, **_kwargs): pass
                        def getresponse(self):
                            if phase == "headers":
                                while True: time.sleep(0.01)
                            return Response()
                        def close(self): pass
                    return sync.fetch_feed(source, validators, resolver, Connection)
                start = time.monotonic()
                with self.assertRaisesRegex(sync.SyncError, "^sync_deadline$"):
                    self.bounded(fetch)
                self.assertLess(time.monotonic() - start, 2)
                with contextlib.closing(sqlite3.connect(self.path)) as db:
                    self.assertEqual(db.execute("SELECT etag FROM sources").fetchone()[0], '"fixture-v1"')
                    self.assertEqual(db.execute("SELECT count(*) FROM revisions").fetchone()[0], 1)
                    self.assertEqual(db.execute("PRAGMA integrity_check").fetchone()[0], "ok")

    def test_deadline_kills_grandchild_and_reaps_direct_child(self):
        pidfile = self.root / "worker-pids.json"
        marker = self.root / "must-not-appear"
        def fetch(*_):
            process = subprocess.Popen([sys.executable, "-c",
                "import pathlib,sys,time;time.sleep(2);pathlib.Path(sys.argv[1]).write_text('leaked')", str(marker)])
            pidfile.write_text(json.dumps([os.getpid(), process.pid]))
            while True: time.sleep(0.01)
        with self.assertRaisesRegex(sync.SyncError, "^sync_deadline$"):
            self.bounded(fetch)
        parent, grandchild = json.loads(pidfile.read_text())
        with self.assertRaises(ChildProcessError): os.waitpid(parent, os.WNOHANG)
        # An adopted grandchild can briefly remain a zombie awaiting init; that
        # is not a running process able to execute a request after the deadline.
        deadline = time.monotonic() + 1
        while True:
            try: state = Path(f"/proc/{grandchild}/stat").read_text().rsplit(")", 1)[1].split()[0]
            except FileNotFoundError: break
            if state == "Z": break
            if time.monotonic() > deadline: self.fail("grandchild still running after group kill")
            time.sleep(0.01)
        self.assertFalse(marker.exists())

    def test_interrupted_transaction_never_promotes_items_or_validators(self):
        self.run_sync([item()])
        def interrupted(db, source, items, now, reply):
            # Transaction has changes in items/search and validators but never
            # commits. Killing the child must release the writer and recover WAL.
            db.execute("UPDATE items SET body='PARTIAL',body_text='PARTIAL'")
            db.execute("UPDATE sources SET etag='PARTIAL'")
            while True: time.sleep(0.01)
        with patch.object(sync, "apply_items", side_effect=interrupted):
            with self.assertRaisesRegex(sync.SyncError, "^sync_deadline$"):
                self.bounded(lambda *_: response([item("second")]))
        with contextlib.closing(sqlite3.connect(self.path)) as db:
            self.assertEqual(db.execute("SELECT etag FROM sources").fetchone()[0], '"fixture-v1"')
            self.assertEqual(db.execute("SELECT body FROM items").fetchone()[0], "searchable collaboration")
            self.assertEqual(db.execute("SELECT count(*) FROM revisions").fetchone()[0], 1)
            self.assertEqual(db.execute("PRAGMA integrity_check").fetchone()[0], "ok")
        self.assertEqual(self.query("PARTIAL"), [])
        self.assertEqual(self.bounded(lambda *_: response([item("second")]))[0]["changed"], 1)

    def test_timeout_cleanup_purges_newly_expired_storage_and_unselected_source(self):
        registry = deepcopy(self.registry)
        second = deepcopy(registry["sources"][0]); second.update(id="second", feed_url="https://second.example/feed")
        registry["sources"].append(second)
        self.run_sync([item()], registry)
        for source in registry["sources"]:
            source["permissions"]["full_text_storage"]["expires_at"] = "2026-09-05T00:00:01Z"
        # Child's hanging fetch cannot reach its finally sweep. The supervisor's
        # fresh cleanup process must use current time and sweep the whole registry.
        def hanging(*_):
            while True: time.sleep(0.01)
        with patch.object(sync, "RETENTION_SECONDS", 0.5), patch.object(sync, "REAP_SECONDS", 0.1):
            # Separate forks inherit this local clock. Parent time is advanced by
            # the supervisor after the collection child is interrupted.
            original = sync._supervised_call
            calls = 0
            def supervise(task, deadline, reap_deadline):
                nonlocal calls
                calls += 1
                with patch.object(sync.time, "time", return_value=NOW if calls == 1 else NOW+2):
                    return original(task, deadline, reap_deadline)
            with patch.object(sync, "_supervised_call", side_effect=supervise):
                with self.assertRaisesRegex(sync.SyncError, "^sync_deadline$"):
                    sync._synchronize_supervised(registry, self.path, "forum", None, hanging, 1.0)
        with contextlib.closing(sqlite3.connect(self.path)) as db:
            self.assertEqual(db.execute("SELECT count(*) FROM items WHERE body IS NOT NULL").fetchone()[0], 0)
            self.assertEqual(db.execute("SELECT count(*) FROM revisions WHERE body IS NOT NULL").fetchone()[0], 0)
            self.assertEqual(db.execute("SELECT count(*) FROM sources WHERE etag IS NOT NULL").fetchone()[0], 0)

    def test_cleanup_failure_is_explicit_and_untrusted_errors_are_sanitized(self):
        self.run_sync([item()])
        def fail(*_): raise sync.SyncError("SECRET_SOURCE_TOKEN_SHOULD_NOT_APPEAR")
        result = self.bounded(fail)
        self.assertEqual(result[0]["error"], "sync_failed")
        def hanging(*_):
            while True: time.sleep(0.01)
        started = time.monotonic()
        with patch.object(sync, "_retention_only", side_effect=hanging):
            with self.assertRaisesRegex(sync.SyncError, "^sync_interrupted_retention_pending$"):
                self.bounded(hanging)
        self.assertLess(time.monotonic()-started, 2)

    def test_disabled_sources_cannot_make_supervised_requests(self):
        registry = deepcopy(self.registry); registry["sources"][0]["enabled"] = False
        result = self.bounded(lambda *_: (_ for _ in ()).throw(AssertionError("disabled request")), registry)
        self.assertEqual(result[0]["status"], "permission_blocked")

    def test_real_http_parser_trickled_headers_and_chunk_framing(self):
        for phase in ("headers", "chunk_framing"):
            with self.subTest(phase=phase):
                self.run_sync([item()])
                def fetch(source, validators):
                    prefix = (b"HTTP/1.1 200 OK\r\nX-Slow: " if phase == "headers" else
                              b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n1;")
                    writer = "import os,sys,time;os.write(1,bytes.fromhex(sys.argv[1]));\nwhile True: os.write(1,b'x');time.sleep(.01)"
                    stream = subprocess.Popen([sys.executable, "-c", writer, prefix.hex()], stdout=subprocess.PIPE)
                    class Socket:
                        def makefile(self, *_args, **_kwargs): return stream.stdout
                    class Connection:
                        def __init__(self, *_): self.reply = None
                        def request(self, *_args, **_kwargs): pass
                        def getresponse(self):
                            self.reply = sync.http.client.HTTPResponse(Socket())
                            self.reply.begin()
                            return self.reply
                        def close(self):
                            if self.reply: self.reply.close()
                    return sync.fetch_feed(source, validators, lambda _: ["1.1.1.1"], Connection)
                started = time.monotonic()
                with self.assertRaisesRegex(sync.SyncError, "^sync_deadline$"):
                    self.bounded(fetch)
                self.assertLess(time.monotonic()-started, 2)
                self.assertEqual(len(self.query("collaboration")), 1)

    def test_prior_complete_source_batch_survives_later_source_deadline(self):
        registry = deepcopy(self.registry)
        second = deepcopy(registry["sources"][0]); second.update(id="second", feed_url="https://second.example/feed")
        registry["sources"].append(second)
        def fetch(source, _validators):
            if source["id"] == "forum": return response([item()])
            while True: time.sleep(0.01)
        with self.assertRaisesRegex(sync.SyncError, "^sync_deadline$"):
            self.bounded(fetch, registry)
        with contextlib.closing(sqlite3.connect(self.path)) as db:
            self.assertEqual(db.execute("SELECT source_id,body FROM items").fetchall(), [("forum", "searchable collaboration")])
            self.assertEqual(db.execute("SELECT id,etag FROM sources ORDER BY id").fetchall(), [("forum", '"fixture-v1"'), ("second", None)])

    def test_worker_output_is_bounded_and_raw_errors_are_not_returned(self):
        deadline = time.monotonic()+1
        with self.assertRaisesRegex(sync.SyncError, "^worker_output_limit$"):
            sync._supervised_call(lambda: "x"*(sync.MAX_WORKER_OUTPUT+1), deadline, deadline+.2)
        class SensitiveError(Exception): pass
        def task(): raise SensitiveError("SECRET_PRIVATE_BODY")
        deadline = time.monotonic()+1
        with self.assertRaisesRegex(sync.SyncError, "^sync_failed$"):
            sync._supervised_call(task, deadline, deadline+.2)

    def test_signal_interruption_restores_handler_and_requests_retention_cleanup(self):
        previous = signal.getsignal(signal.SIGTERM)
        for interrupt in ("term", "keyboard"):
            with self.subTest(interrupt=interrupt):
                calls = 0
                def supervise(task, deadline, reap_deadline):
                    nonlocal calls
                    calls += 1
                    if calls == 1:
                        if interrupt == "keyboard": raise KeyboardInterrupt()
                        handler = signal.getsignal(signal.SIGTERM)
                        self.assertTrue(callable(handler))
                        handler(signal.SIGTERM, None)
                    return None
                with patch.object(sync, "_supervised_call", side_effect=supervise):
                    with self.assertRaisesRegex(sync.SyncError, "^sync_interrupted$"):
                        sync.synchronize(self.registry, self.path)
                self.assertEqual(calls, 2)
                self.assertEqual(signal.getsignal(signal.SIGTERM), previous)


if __name__ == "__main__": unittest.main()
