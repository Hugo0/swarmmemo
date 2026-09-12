"""Synthetic-only projection, publication barrier and collector-guard fixtures."""
import copy
from contextlib import closing
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

import publish_references as pub
import sync_sources as sync

NOW = 1788566400


def policy_value():
    grant = {"approved": True, "evidence_url": "https://permission.example/ticket?private=DO-NOT-REFLECT",
             "reviewed_at": "2026-09-04T00:00:00Z", "expires_at": "2026-09-06T00:00:00Z",
             "scope": "Synthetic test permission; not real source approval"}
    return {"version": 1, "sources": [{"id": "fixture", "name": "Fixture reference feed",
            "adapter": "jsonfeed-1.1", "feed_url": "https://example.org/feed.json", "enabled": True,
            "permissions": {right: copy.deepcopy(grant) for right in ("automated_collection", "full_text_storage", "public_archive")}}]}


def item(external_id="first", text="Fixture excerpt 雪"):
    return {"id": external_id, "url": "https://example.org/" + external_id, "title": "Reference title",
            "content_text": text, "date_published": "2026-09-04T11:12:13+01:00",
            "authors": [{"name": "External fixture author", "url": "https://example.org/author"}]}


def response(items):
    return {"status": 200, "body": pub.canonical({"version": sync.FEED_VERSION, "title": "Synthetic", "items": items}),
            "etag": '"fixture"'}


class PublicationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="reference-publication-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.catalog = self.root / "catalog.sqlite"
        self.registry = self.root / "registry.json"
        self.suppression = self.root / "suppression.json"
        self.snapshot = self.root / "snapshot.json"
        self.policy = policy_value()
        self.write_policy()
        self.write(self.suppression, pub.canonical({"version": 1, "ids": []}))

    def write(self, path, data):
        path.write_bytes(data); path.chmod(0o600)

    def write_policy(self): self.write(self.registry, pub.canonical(self.policy))

    def publish(self, items=None, *, collect=True, now=NOW, failure=None, response_value=None):
        def fetch(*_):
            if failure: raise failure
            return response_value if response_value is not None else response([item()] if items is None else items)
        with patch.object(sync, "fetch_feed", fetch), patch.object(pub, "_now", lambda: now), patch.object(sync.time, "time", lambda: now):
            return pub.publish(self.catalog, self.registry, self.suppression, self.snapshot, collect=collect, total_timeout=10)

    def validated(self, now=NOW):
        return pub.validate_snapshot(self.snapshot.read_bytes(), self.registry.read_bytes(), self.suppression.read_bytes(), now)

    def test_first_collect_marker_exact_projection_and_no_evidence_leak(self):
        result = self.publish()
        current = self.validated()
        self.assertEqual(result["references"], 1)
        self.assertEqual(len(current["sources"][0]), 8)
        self.assertEqual(len(current["references"][0]), 18)
        self.assertEqual((self.root / "catalog.sqlite.publication-bound.json").read_bytes(), b'{"version":1}')
        self.assertEqual(self.snapshot.stat().st_mode & 0o777, 0o640)
        self.assertEqual(self.catalog.stat().st_mode & 0o777, 0o600)
        self.assertNotIn(b"DO-NOT-REFLECT", self.snapshot.read_bytes())
        with closing(sqlite3.connect(self.catalog)) as db:
            self.assertEqual(current["references"][0]["content_hash"], db.execute("SELECT content_hash FROM items").fetchone()[0])
        self.assertFalse(current["references"][0]["native_identity"])
        self.assertFalse(current["references"][0]["claimable_job"])
        self.assertFalse(current["references"][0]["hugging_face_eligible"])

    def test_dry_run_does_not_bind_create_catalog_or_network(self):
        with patch.object(sync, "fetch_feed", side_effect=AssertionError("network")):
            with patch("builtins.print"):
                self.assertEqual(pub.main(["--catalog", str(self.catalog), "--registry", str(self.registry),
                    "--suppression", str(self.suppression), "--snapshot", str(self.snapshot)]), 0)
        self.assertFalse(self.catalog.exists())
        self.assertFalse(self.snapshot.exists())
        self.assertFalse((self.root / "catalog.sqlite.publication-bound.json").exists())

    def test_distributed_disabled_registry_and_optional_labels_match_reader(self):
        self.write(self.registry, Path(sync.__file__).with_name("sync-sources.example.json").read_bytes())
        with patch.object(sync, "fetch_feed", side_effect=AssertionError("disabled source fetched")):
            result = pub.publish(self.catalog, self.registry, self.suppression, self.snapshot, collect=True)
        self.assertEqual((result["sources"], result["references"]), (0, 0))
        del self.policy["sources"][0]["name"]
        self.policy["sources"][0]["terms_url"] = None
        self.write_policy()
        self.publish()
        self.assertEqual(self.validated()["sources"][0]["name"], "")

    def test_refresh_requires_valid_ready_and_never_fetches(self):
        for raw in (None, pub.canonical({"version": 1, "state": "blocked"})):
            if raw: self.write(self.snapshot, raw)
            with self.assertRaises(pub.PublicationError): self.publish(collect=False)
        self.publish()
        with patch.object(sync, "fetch_feed", side_effect=AssertionError("network")), patch.object(pub, "_now", lambda: NOW + 10):
            result = pub.publish(self.catalog, self.registry, self.suppression, self.snapshot)
        self.assertEqual(result["generated_at"], NOW + 10)
        self.assertEqual(self.validated(NOW + 10)["sources"][0]["last_successful_at"], NOW)
        with self.assertRaisesRegex(pub.PublicationError, "publication_stale"): self.publish(collect=False, now=NOW + 1000)

    def test_failed_collection_then_refresh_stays_blocked_until_success(self):
        self.publish()
        with self.assertRaisesRegex(pub.PublicationError, "source_failed"): self.publish(failure=TimeoutError("SECRET"))
        self.assertEqual(self.snapshot.read_bytes(), b'{"state":"blocked","version":1}')
        with self.assertRaisesRegex(pub.PublicationError, "publication_blocked"): self.publish(collect=False)
        self.publish()
        self.assertEqual(len(self.validated()["references"]), 1)

    def test_ordinary_api_and_cli_refuse_bound_and_dangling_markers(self):
        self.publish()
        normalized = sync.load_registry(self.registry)
        with self.assertRaisesRegex(sync.SyncError, "publication_guard_required"):
            sync.synchronize(normalized, self.catalog, fetcher=lambda *_: self.fail("network"))
        marker = self.root / "catalog.sqlite.publication-bound.json"
        marker.unlink(); marker.symlink_to(self.root / "missing")
        with self.assertRaisesRegex(sync.SyncError, "publication_guard_required"):
            sync.synchronize(normalized, self.catalog, fetcher=lambda *_: self.fail("network"))
        run = subprocess.run([sys.executable, "-B", str(Path(sync.__file__)), "--registry", str(self.registry),
                              "--catalog", str(self.catalog), "--sync"], capture_output=True, timeout=5,
                             env={**os.environ, "PYTHONDONTWRITEBYTECODE": "1"})
        self.assertEqual(run.returncode, 1)
        self.assertIn(b"publication_guard_required", run.stdout + run.stderr)

    def test_unbound_api_still_works_and_lock_serializes_activation(self):
        normalized = sync.load_registry(self.registry)
        result = sync.synchronize(normalized, self.catalog, now=NOW, fetcher=lambda *_: response([item()]))
        self.assertEqual(result[0]["status"], "ok")
        with sync.publication_lock(self.catalog):
            with self.assertRaisesRegex(pub.PublicationError, "publication_busy"): self.publish()
            with self.assertRaisesRegex(sync.SyncError, "publication_busy"):
                sync.synchronize(normalized, self.catalog, now=NOW, fetcher=lambda *_: self.fail("network"))

    def test_policy_disable_removal_hash_expiry_and_metadata_only(self):
        self.publish()
        original = self.snapshot.read_bytes()
        self.policy["sources"][0]["enabled"] = False; self.write_policy()
        with self.assertRaisesRegex(pub.PublicationError, "publication_changed"):
            pub.validate_snapshot(original, self.registry.read_bytes(), self.suppression.read_bytes(), NOW)
        with self.assertRaises(pub.PublicationError): self.publish(collect=False)
        self.publish()
        self.assertEqual(self.validated()["references"], [])
        with closing(sqlite3.connect(self.catalog)) as db:
            self.assertEqual(db.execute("SELECT count(*) FROM items WHERE body IS NOT NULL").fetchone()[0], 0)
        self.policy["sources"][0]["enabled"] = True
        self.policy["sources"][0]["permissions"]["full_text_storage"]["approved"] = False; self.write_policy()
        self.publish()
        ref = self.validated()["references"][0]
        self.assertEqual((ref["excerpt"], ref["excerpt_available"], ref["excerpt_truncated"]), ("", False, False))
        with closing(sqlite3.connect(self.catalog)) as db:
            self.assertEqual(db.execute("SELECT status,body_mode,etag,last_modified FROM sources").fetchone(), ("ok", 0, None, None))
            self.assertEqual(db.execute("SELECT count(*) FROM items WHERE body IS NOT NULL OR body_text!=''").fetchone()[0], 0)
            self.assertEqual(db.execute("SELECT count(*) FROM revisions WHERE body IS NOT NULL OR body_text!=''").fetchone()[0], 0)
        self.policy["sources"] = []; self.write_policy(); self.publish()
        self.assertEqual(self.validated()["sources"], [])

    def test_policy_change_during_projection_blocks_and_exact_bytes_matter(self):
        self.publish()
        old = self.registry.read_bytes()
        self.write(self.registry, old + b"\n")
        with self.assertRaisesRegex(pub.PublicationError, "publication_changed"): self.publish(collect=False)
        self.publish()
        original = pub._project
        def changed(*args):
            result = original(*args)
            self.write(self.registry, old + b"  ")
            return result
        with patch.object(pub, "_project", changed):
            with self.assertRaisesRegex(pub.PublicationError, "publication_changed"): self.publish()
        self.assertEqual(self.snapshot.read_bytes(), b'{"state":"blocked","version":1}')

    def test_suppression_and_explicit_tombstones_never_resurrect(self):
        self.publish()
        rid = self.validated()["references"][0]["id"]
        self.write(self.suppression, pub.canonical({"version": 1, "ids": [rid]}))
        with self.assertRaises(pub.PublicationError): self.publish(collect=False)
        self.publish(); self.assertEqual(self.validated()["references"], [])
        self.write(self.suppression, pub.canonical({"version": 1, "ids": []}))
        self.publish([{ "id": "first", "_swarmmemo": {"deleted": True}}])
        self.assertEqual(self.validated()["references"], [])
        self.publish([item(text="STALE REAPPEARANCE")]); self.assertEqual(self.validated()["references"], [])
        with closing(sqlite3.connect(self.catalog)) as db:
            self.assertEqual(db.execute("SELECT count(*) FROM revisions WHERE body IS NOT NULL").fetchone()[0], 0)

    def test_304_missing_and_source_freshness_do_not_invent_item_observation(self):
        self.publish()
        self.publish(response_value={"status": 304}, now=NOW + 30)
        current = self.validated(NOW + 30)
        self.assertEqual(current["sources"][0]["last_successful_at"], NOW + 30)
        self.assertEqual(current["references"][0]["last_observed_at"], NOW)
        self.publish([], now=NOW + 40)
        self.assertEqual(self.validated(NOW + 40)["references"][0]["last_observed_at"], NOW)
        with self.assertRaises(pub.PublicationError): self.publish(collect=False, now=NOW + 86441)

    def test_cuttle_exact_excerpt_attribution_unknown_timezone_and_bounds(self):
        source = self.policy["sources"][0]
        source.update(adapter=sync.CUTTLE_ADAPTER, feed_url=sync.CUTTLE_FEED)
        self.write_policy()
        row = {"slug": "sample", "url": sync.CUTTLE_ORIGIN + "/sample/", "title": "Co-created index",
               "excerpt": "雪" * 600, "published_at": "2026-09-04 12:00:00", "series": "weekly-digests",
               "6529_learning_path": None, "agents_learning_path": None}
        self.publish(response_value={"status": 200, "body": pub.canonical([row])})
        ref = self.validated()["references"][0]
        self.assertEqual(ref["excerpt"], "雪" * 512)
        self.assertTrue(ref["excerpt_truncated"])
        self.assertFalse(ref["source_publication_timezone_known"])
        self.assertEqual(len(ref["authors"]), 2)
        with closing(sqlite3.connect(self.catalog)) as db, db:
            db.execute("UPDATE items SET body_text='forged prefix'")
        with self.assertRaisesRegex(pub.PublicationError, "invalid_excerpt"): self.publish(collect=False)
        self.assertEqual(self.snapshot.read_bytes(), b'{"state":"blocked","version":1}')

    def test_source_rebinding_fails_and_permissions_expire_before_final_ready(self):
        self.publish()
        self.policy["sources"][0]["feed_url"] = "https://elsewhere.example/feed.json"; self.write_policy()
        with self.assertRaises(pub.PublicationError): self.publish()
        self.assertEqual(self.snapshot.read_bytes(), b'{"state":"blocked","version":1}')
        self.policy = policy_value(); self.write_policy(); self.publish()
        raw = self.snapshot.read_bytes()
        with self.assertRaises(pub.PublicationError):
            pub.validate_snapshot(raw, self.registry.read_bytes(), self.suppression.read_bytes(), NOW + 86400)

    def test_strict_files_urls_json_and_aliasing(self):
        for value in (b'{"version":true,"sources":[]}', b'{"version":1e0,"sources":[]}',
                      b'{"version":-0,"sources":[]}', b'{"version":1,"version":1,"sources":[]}',
                      b'{"version":1,"sources":[],"bad":"\\ud800"}'):
            with self.assertRaises(pub.PublicationError): pub.registry(value)
        for url in ("https://example.org?", "https://@example.org", "https://example.org#", "https://127.0.0.1/x", "https://[::1]/", "https://localhost/", "https://雪.example/x", "https://example.org:444/"):
            with self.assertRaises(pub.PublicationError): pub.public_url(url)
        self.registry.chmod(0o666)
        with self.assertRaises(pub.PublicationError): self.publish()
        self.registry.chmod(0o600)
        with self.assertRaisesRegex(pub.PublicationError, "unsafe_path"):
            pub.publish(self.catalog, self.registry, self.suppression, self.registry, collect=True)
        self.snapshot.symlink_to(self.registry)
        with self.assertRaises(pub.PublicationError): self.publish()

    def test_blocked_fsync_failure_prevents_collection_and_ready_uncertainty_is_reported(self):
        original = pub._atomic
        def blocked_fail(path, raw, mode=0o640):
            if Path(path) == self.snapshot and b'"blocked"' in raw: raise pub.PublicationError("durability_uncertain")
            return original(path, raw, mode)
        with patch.object(pub, "_atomic", blocked_fail), patch.object(sync, "_synchronize", side_effect=AssertionError("must not collect")):
            with self.assertRaisesRegex(pub.PublicationError, "durability_uncertain"): self.publish()
        def ready_fail(path, raw, mode=0o640):
            original(path, raw, mode)
            if Path(path) == self.snapshot and b'"ready"' in raw: raise pub.PublicationError("durability_uncertain")
        with patch.object(pub, "_atomic", ready_fail):
            with self.assertRaisesRegex(pub.PublicationError, "durability_uncertain"): self.publish()
        self.assertEqual(self.snapshot.read_bytes(), b'{"state":"blocked","version":1}')

    def test_real_process_crash_after_blocked_or_ready_installation(self):
        self.publish()
        for phase in ("blocked", "ready"):
            code = '''import os,sys,json
from pathlib import Path
sys.path.insert(0,sys.argv[1])
import publish_references as p, sync_sources as s
p._now=lambda:1788566400
s.time.time=lambda:1788566400
s.fetch_feed=lambda *_:{"status":304}
original=s._supervised_call
def crash(*args):
 result=original(*args)
 if ('"'+sys.argv[6]+'"').encode() in Path(sys.argv[5]).read_bytes():os._exit(79)
 return result
s._supervised_call=crash
p.publish(*sys.argv[2:6],collect=True,total_timeout=10)
'''
            result = subprocess.run([sys.executable, "-B", "-c", code, str(Path(pub.__file__).parent),
                str(self.catalog), str(self.registry), str(self.suppression), str(self.snapshot), phase], timeout=8,
                capture_output=True, env={**os.environ, "PYTHONDONTWRITEBYTECODE": "1"})
            self.assertEqual(result.returncode, 79, result.stderr)
            if phase == "blocked":
                with self.assertRaisesRegex(pub.PublicationError, "publication_blocked"): self.publish(collect=False)
            else: self.validated()
            self.publish()

    def test_preconditions_refuse_before_any_file_write_for_both_modes(self):
        for collect in (False, True):
            with patch.object(sync.threading, "active_count", lambda: 2):
                with self.assertRaisesRegex(pub.PublicationError, "worker_requires_single_thread"):
                    self.publish(collect=collect)
            previous = signal.signal(signal.SIGCHLD, signal.SIG_IGN)
            try:
                with self.assertRaisesRegex(pub.PublicationError, "worker_requires_default_sigchld"):
                    self.publish(collect=collect)
            finally: signal.signal(signal.SIGCHLD, previous)
        self.assertFalse(self.catalog.exists())
        self.assertFalse(self.snapshot.exists())
        self.assertFalse((self.root / "catalog.sqlite.publication.lock").exists())
        self.assertFalse((self.root / "catalog.sqlite.publication-bound.json").exists())

    def test_writable_ancestors_and_special_files_fail_closed(self):
        unsafe = self.root / "writable"; unsafe.mkdir(mode=0o700)
        child = unsafe / "protected"; child.mkdir(mode=0o700)
        unsafe.chmod(0o777)
        for mode in (0o777, 0o1777):
            unsafe.chmod(mode)
            with self.assertRaisesRegex(pub.PublicationError, "unsafe_path"): pub.checked_path(child / "snapshot")
            with self.assertRaisesRegex(sync.SyncError, "publication_path_unsafe"):
                with sync.publication_lock(child / "catalog"): self.fail("unsafe path accepted")
        unsafe.chmod(0o700)
        pub.checked_path(child / "snapshot")
        fifo = self.root / "fifo"; os.mkfifo(fifo, 0o600)
        with self.assertRaisesRegex(pub.PublicationError, "unsafe_path"): pub.read_file(fifo, 100)

    def test_projection_phase_is_killable_and_offline_refresh_stays_blocked(self):
        self.publish()
        pid_path = self.root / "projection.pid"
        def hung(*_):
            pid_path.write_text(str(os.getpid()))
            time.sleep(60)
        started = time.monotonic()
        with patch.object(pub, "_project", hung):
            with self.assertRaisesRegex(pub.PublicationError, "publication_deadline"):
                self.publish(collect=False)
        self.assertLess(time.monotonic() - started, 11)
        self.assertEqual(self.snapshot.read_bytes(), b'{"state":"blocked","version":1}')
        with self.assertRaises(ProcessLookupError): os.kill(int(pid_path.read_text()), 0)
        with self.assertRaisesRegex(pub.PublicationError, "publication_blocked"): self.publish(collect=False)

    def test_final_permission_clock_recheck_and_capacity_fail_closed(self):
        self.policy["sources"][0]["permissions"]["public_archive"]["expires_at"] = "2026-09-05T00:00:01Z"
        self.write_policy()
        original = pub._project
        def expire(*args):
            raw = original(*args)
            pub._now = lambda: NOW + 2
            return raw
        with patch.object(pub, "_project", expire):
            with self.assertRaisesRegex(pub.PublicationError, "publication_stale"): self.publish()
        self.assertEqual(self.snapshot.read_bytes(), b'{"state":"blocked","version":1}')
        self.policy = policy_value(); self.write_policy()
        with patch.object(pub, "MAX_BYTES", 1024):
            with self.assertRaisesRegex(pub.PublicationError, "capacity_exceeded"): self.publish()
        self.assertEqual(self.snapshot.read_bytes(), b'{"state":"blocked","version":1}')


class VectorTests(unittest.TestCase):
    def test_explicit_depth_and_node_bounds(self):
        value = 1
        for _ in range(32): value = [value]
        self.assertEqual(pub.strict_json(pub.canonical(value)), value)
        with self.assertRaises(pub.PublicationError): pub.canonical([value])
        with self.assertRaises(pub.PublicationError): pub.strict_json(b'[' * 33 + b'1' + b']' * 33)
        value = [None] * 149999
        self.assertEqual(len(pub.strict_json(pub.canonical(value))), 149999)
        with self.assertRaises(pub.PublicationError): pub.canonical(value + [None])
        with self.assertRaises(pub.PublicationError): pub.strict_json(b'[' + b'null,' * 149999 + b'null]')

    def test_shared_vectors(self):
        vectors = json.loads(Path(__file__).with_name("reference-vectors.json").read_bytes())
        self.assertEqual(vectors["version"], 1)
        for vector in vectors["id_vectors"]:
            raw = pub.canonical(["swarmmemo.reference.v1", vector["source_id"], vector["external_id"]])
            self.assertEqual(raw.hex(), vector["canonical_utf8_hex"])
            self.assertEqual(pub.reference_id(vector["source_id"], vector["external_id"]), vector["id"])
        for vector in vectors["encoding_vectors"]:
            raw = pub.canonical(vector["value"])
            self.assertEqual(raw.hex(), vector["canonical_utf8_hex"])
            self.assertEqual(pub.sha(raw), vector["sha256"])
        for vector in vectors["reader_vectors"]:
            with self.subTest(vector=vector["name"]):
                args = (vector["projection_utf8"].encode(), vector["registry_utf8"].encode(), vector["suppression_utf8"].encode(), vector["now"])
                if vector["valid"]: pub.validate_snapshot(*args)
                else:
                    with self.assertRaises(pub.PublicationError): pub.validate_snapshot(*args)


if __name__ == "__main__": unittest.main()
