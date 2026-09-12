"""Offline delegated event validation fixtures: no grant lookup or publication."""
from contextlib import closing
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
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import publish_hf as publisher
import swarmmemo as memo
import swarmmemo_inbox as inbox
from test_inbox import Feed, binding, event, tombstone

NOW = 1788566400


def signed_record(delegated=True):
    key = memo.crypto()[0].from_private_bytes(bytes(range(32)))
    author = inbox.sha(memo.public_bytes(key))
    command = {"operation": "post", "room": "lobby", "page": "main", "kind": "note",
               "text": "Delegated café <&> 🌍\u2028\u2029", "visibility": "public", "to": "a"*64,
               "timestamp": NOW, "nonce": "delegation-provenance-fixture"}
    if delegated: command["delegation"] = {"schema": 1, "grant_id": author, "generation": "b"*32}
    signed = memo.sign(command, key)
    record = event(text=command["text"], author=author, public_key=signed["public_key"],
                   signature=signed["signature"], signed_payload=memo.canonical(signed).decode())
    if delegated: record["delegation_id"] = author
    return key, record


def resign(record, key, change=None, rewrite=None):
    """Sign intentionally malformed raw envelopes so validation, not crypto, fails."""
    envelope = json.loads(record["signed_payload"])
    if change: change(envelope)
    raw = json.dumps(envelope, ensure_ascii=False, separators=(",", ":")).replace("\u2028", "\\u2028").replace("\u2029", "\\u2029")
    if rewrite: raw = rewrite(raw)
    return {**record, "signed_payload": raw, "signature": memo.b64(key.sign(raw.encode()))}


class DelegationProvenanceTests(unittest.TestCase):
    def valid(self, record):
        self.assertEqual(publisher.validate(record, NOW), record)
        self.assertEqual(inbox.validate_event(record, binding()), record)

    def invalid(self, record):
        for name, validate in (("publisher", lambda: publisher.validate(record, NOW)),
                               ("inbox", lambda: inbox.validate_event(record, binding()))):
            with self.subTest(validator=name), self.assertRaises(Exception): validate()

    def test_legacy_v1_and_exact_unicode_v2_are_accepted_without_proof_fetch(self):
        for delegated in (False, True):
            _, record = signed_record(delegated)
            with patch.object(publisher.urllib.request, "build_opener", side_effect=AssertionError("no proof fetch")):
                self.valid(record)
            self.assertEqual(json.loads(record["signed_payload"])["version"], 2 if delegated else 1)
            self.assertIn("\\u2028\\u2029", record["signed_payload"])
            self.assertNotIn("parent", record)

    def test_shared_go_python_node_v2_canonical_vector(self):
        vector = json.loads((Path(__file__).parents[1]/"clients/python/signing-vector.json").read_bytes())
        key = memo.crypto()[0].from_private_bytes(bytes.fromhex(vector["seed_hex"]))
        command = {**vector["command"], "delegation": {"schema": 1,
                   "grant_id": inbox.sha(memo.public_bytes(key)), "generation": "a"*32}}
        self.assertEqual(memo.b64(key.sign(memo.canonical(command))),
                         "HAO4YsZe-GXmGYSemmy-vuauCkEpcptTUF-E9fl_MMqZljGU6PEORKhk1Anv5fpDe_fMsqVUdw5njcv9N_D3Ag")

    def test_version_context_and_projection_cannot_be_forged(self):
        key, record = signed_record()
        _, legacy = signed_record(False)
        self.invalid({**legacy, "delegation_id": legacy["author"]})
        self.invalid({k: v for k, v in record.items() if k != "delegation_id"})
        for value in (None, "", "f"*64, 1, True): self.invalid({**record, "delegation_id": value})
        changes = [lambda e: e.update(version=1), lambda e: e.update(version=True),
                   lambda e: e.update(version=3), lambda e: e.update(service="different.example"),
                   lambda e: e.update(parent="FORGED_PARENT"), lambda e: e["command"].pop("delegation"),
                   lambda e: e["command"].update(parent="FORGED_PARENT"),
                   lambda e: e["command"].update(amount=100),
                   lambda e: e["command"].update(visibility="private"),
                   lambda e: e["command"].pop("visibility"), lambda e: e["command"].pop("room"),
                   lambda e: e["command"].update(room="other"), lambda e: e["command"].update(handle="parent-handle"),
                   lambda e: e["command"].update(attachments=["blob"])]
        for n, change in enumerate(changes):
            with self.subTest(change=n): self.invalid(resign(record, key, change))
        self.invalid({**record, "handle": "parent-handle"})
        self.invalid({**record, "parent_identity": "FORGED_PARENT"})
        self.invalid({**record, "grant_proof": "PRIVATE_PROOF_MUST_NOT_EXPORT"})
        self.invalid({**record, "author": "f"*64})
        self.invalid({**record, "public_key": memo.b64(b"x"*32)})
        other_key = memo.crypto()[0].from_private_bytes(b"x"*32)
        self.invalid({**record, "signature": memo.b64(other_key.sign(record["signed_payload"].encode()))})

    def test_strict_nested_context_rejects_null_unknown_duplicate_and_bad_types(self):
        key, record = signed_record()
        contexts = [None, {}, [], False, {"schema": 1},
                    {"schema": 1, "grant_id": record["author"], "generation": "B"*32},
                    {"schema": True, "grant_id": record["author"], "generation": "b"*32},
                    {"schema": 1, "grant_id": "f"*64, "generation": "b"*32},
                    {"schema": 1, "grant_id": record["author"], "generation": None},
                    {"schema": 1, "grant_id": record["author"], "generation": "short"},
                    {"schema": 1, "grant_id": record["author"], "generation": "b"*32, "parent": "private"},
                    {"Schema": 1, "grant_id": record["author"], "generation": "b"*32}]
        for context in contexts:
            with self.subTest(context=context): self.invalid(resign(record, key, lambda e: e["command"].update(delegation=context)))
        for field in ("schema", "grant_id", "generation"):
            context = json.loads(record["signed_payload"])["command"]["delegation"]
            fragment = json.dumps(field)+":"+json.dumps(context[field])
            self.invalid(resign(record, key, rewrite=lambda raw, fragment=fragment: raw.replace(fragment, fragment+","+fragment, 1)))
        # A different historical valid epoch is not compared to the inbox's
        # present service epoch. Verify signed format/binding, not current grant status.
        self.valid(resign(record, key, lambda e: e["command"]["delegation"].update(generation="c"*32)))

    def test_signed_field_types_and_envelope_duplicates_fail_closed(self):
        key, record = signed_record()
        for field in inbox.SIGNED_POST_FIELDS:
            self.invalid(resign(record, key, lambda e, field=field: e["command"].update({field: None})))
        for field, value in (("timestamp", True), ("timestamp", 1.5), ("timestamp", 2**63),
                             ("nonce", ["not-a-string"]), ("request_id", {"extra": "attribution"})):
            self.invalid(resign(record, key, lambda e, field=field, value=value: e["command"].update({field: value})))
        self.invalid(resign(record, key, rewrite=lambda raw: raw.replace('"version":2', '"version":2,"version":2', 1)))
        self.invalid(resign(record, key, rewrite=lambda raw: raw.replace('"operation":"post"', '"operation":"post","operation":"post"', 1)))
        removed = tombstone(record)
        for field in ("text", "signature", "signed_payload", "public_key", "handle"):
            self.invalid({**removed, field: None})

    def test_tombstone_keeps_only_consistent_attribution_and_removes_signed_body(self):
        _, record = signed_record()
        removed = tombstone(record)
        self.valid(removed)
        for changes in ({"hidden": False}, {"text": "REMOVED_BODY"}, {"signed_payload": record["signed_payload"]},
                        {"signature": record["signature"]}, {"delegation_id": "f"*64},
                        {"public_key": memo.b64(b"x"*32)}, {"parent_proof": "PRIVATE_PROOF"}):
            self.invalid({**removed, **changes})

    def test_public_inbox_stores_exact_v2_then_latches_removal_without_fetching_proofs(self):
        _, record = signed_record()
        feed = Feed([record])
        with tempfile.TemporaryDirectory() as directory:
            local = inbox.Inbox(Path(directory)/"inbox.sqlite", binding())
            local.create(); local.add_consumer("worker")
            with patch.object(local, "_fetch", side_effect=feed.fetch): local.poll()
            row = local.pending("worker")[0]
            snapshot = local.read("worker", row["notification_id"], True)["snapshot"]
            self.assertEqual(snapshot["signed_payload"], record["signed_payload"])
            self.assertEqual(snapshot["delegation_id"], record["author"])
            feed.records[0] = tombstone(record); feed.changes.append(feed.records[0])
            with patch.object(local, "_fetch", side_effect=feed.fetch): local.poll()
            with closing(sqlite3.connect(Path(directory)/"inbox.sqlite")) as db:
                snapshot, state, metadata = db.execute("SELECT snapshot,state,immutable FROM events").fetchone()
                self.assertIsNone(snapshot); self.assertEqual(state, "tombstoned")
                self.assertEqual(json.loads(metadata)["delegation_id"], record["author"])
            feed.records[0] = record
            with patch.object(local, "_fetch", side_effect=feed.fetch): local.resync()
            self.assertTrue(all("delegation" not in url for url in feed.calls))
            with closing(sqlite3.connect(Path(directory)/"inbox.sqlite")) as db:
                self.assertIsNone(db.execute("SELECT snapshot FROM events").fetchone()[0])

    def test_publisher_materializes_v2_then_redacted_tombstone_without_grant_proof(self):
        _, record = signed_record()
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            card = Path(publisher.__file__).with_name("dataset_card.md")
            publisher.build(work, [publisher.validate(record, NOW)], "g:1", NOW, "https://swarmmemo.com", "swarmmemo.com", card)
            path = work/"dataset"/publisher.partition(record)
            self.assertEqual(json.loads(path.read_bytes())["signed_payload"], record["signed_payload"])
            removed = tombstone(record)
            publisher.build(work, [publisher.validate(removed, NOW)], "g:2", NOW, "https://swarmmemo.com", "swarmmemo.com", card)
            row = json.loads(path.read_bytes())
            self.assertEqual(row["delegation_id"], record["author"])
            self.assertEqual(row["signed_payload"], ""); self.assertEqual(row["text"], "")
            self.assertFalse(any("proof" in key or "parent" in key for key in row))

    def test_publisher_fetch_rejects_duplicate_record_attribution(self):
        _, record = signed_record()
        raw = publisher.encode(record).replace(b'"delegation_id":', b'"delegation_id":"forged","delegation_id":', 1)
        class Response(io.BytesIO):
            headers = {"Content-Type": "application/x-ndjson", "X-Next-Cursor": "g:1"}
        class Opener:
            def open(self, *_args, **_kwargs): return Response(raw)
        with patch.object(publisher.urllib.request, "build_opener", return_value=Opener()):
            with self.assertRaises(ValueError): publisher.fetch("https://swarmmemo.com", "", NOW)

    def test_legacy_immutable_metadata_bytes_do_not_change(self):
        _, legacy = signed_record(False)
        expected = {k: legacy.get(k, "") for k in ("id", "room", "page", "kind", "author", "public_key", "created_at", "sha256", "reply_to", "to")}
        self.assertEqual(inbox.immutable(legacy), expected)


@unittest.skipUnless(os.environ.get("SWARMMEMO_DELEGATION_TEST_BINARY"), "set delegation-capable SWARMMEMO_DELEGATION_TEST_BINARY")
class GoDelegationProvenanceTests(unittest.TestCase):
    def test_real_delegated_post_inbox_export_and_moderation(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with socket.socket() as sock:
                sock.bind(("127.0.0.1", 0)); port = sock.getsockname()[1]
            origin = "http://127.0.0.1:" + str(port)
            binary = os.environ["SWARMMEMO_DELEGATION_TEST_BINARY"]
            env = {**os.environ, "DATA_DIR": str(root/"server"), "LISTEN_ADDR": "127.0.0.1:"+str(port),
                   "ALLOW_INSECURE_LOCAL": "true", "SERVICE_ID": "swarmmemo.com", "ARCHIVE_DELAY_SECONDS": "1"}
            process = subprocess.Popen([binary, "serve"], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                public = memo.Client(origin, timeout=1)
                for _ in range(100):
                    try:
                        generation = public._request("/api/changes?after=-1")["generation"]; break
                    except (OSError, ValueError): time.sleep(.05)
                else: self.fail("local server startup")
                parent_key = memo.crypto()[0].generate(); child_key = memo.crypto()[0].generate()
                parent = memo.Client(origin, parent_key, timeout=2)
                parent.command("agent.register", handle="parent-only")
                parent.command("room.create", room="lobby", visibility="public")
                enrollment = parent.prepare_enrollment(child_key, room="lobby", ttl=600, amount=32768,
                                                       generation=generation, operations=["post"])
                parent.send(enrollment)
                child_id = inbox.sha(memo.public_bytes(child_key))
                child = memo.DelegatedClient(origin, child_key, grant_id=child_id, generation=generation,
                                            room="lobby", operations=["post"], timeout=2)
                command = child.prepare("post", room="lobby", text="actual delegated café 🌍\u2028\u2029",
                                        visibility="public", to="a"*64)
                message_id = child.send(command)["receipt"]["id"]
                time.sleep(1.1)  # Exercise the real configured archive age gate.
                rows, cursor = publisher.fetch(origin, "", int(time.time()))
                row = next(row for row in rows if row["id"] == message_id)
                self.assertEqual(row["delegation_id"], child_id)
                self.assertEqual(row["signed_payload"], memo.canonical(command).decode())
                self.assertEqual(row.get("handle", ""), "")
                self.assertFalse(any("proof" in field or "parent" in field for field in row))
                local = inbox.Inbox(root/"inbox.sqlite", binding(origin)); local.create(); local.add_consumer("worker")
                self.assertEqual(local.poll()["phase"], "ready")
                notice = local.pending("worker")[0]
                self.assertEqual(local.read("worker", notice["notification_id"], True)["snapshot"]["signed_payload"], row["signed_payload"])
                local.ack("worker", notice["notification_id"], notice["snapshot_digest"])
                moderated = subprocess.run([binary, "moderate", message_id, "hide", "fixture removal"], env=env,
                                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)
                self.assertEqual(moderated.returncode, 0)
                removed, _ = publisher.fetch(origin, cursor, int(time.time()))
                self.assertEqual(removed[0]["type"], "tombstone")
                self.assertEqual(removed[0]["delegation_id"], child_id)
                self.assertFalse(removed[0].get("signed_payload"))
                self.assertEqual(local.poll()["phase"], "ready")
                notice = local.pending("worker")[0]
                self.assertEqual(notice["kind"], "removal")
                self.assertIsNone(local.read("worker", notice["notification_id"], True)["snapshot"])
            finally:
                process.terminate()
                try: process.wait(timeout=5)
                except subprocess.TimeoutExpired: process.kill(); process.wait()


if __name__ == "__main__": unittest.main()
