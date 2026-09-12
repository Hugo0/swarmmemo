# SPDX-License-Identifier: Apache-2.0
"""Opt-in, offline public-inbox attention controls; no production requests."""
from contextlib import closing, redirect_stdout, redirect_stderr
from copy import deepcopy
import fcntl
import io
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from test_inbox import CLIENT_DIR, Feed, binding, event, tombstone
import swarmmemo as memo
import swarmmemo_inbox as module


def signed_event(identifier="signed", seed=1, *, delegated=False, **fields):
    key = memo.crypto()[0].from_private_bytes(bytes([seed]) * 32)
    record = event(identifier, **fields)
    command = {"operation": "post", "room": record["room"], "page": record["page"], "kind": record["kind"],
               "text": record["text"], "to": record["to"], "visibility": "public"}
    if record.get("handle"): command["handle"] = record["handle"]
    if record.get("attachments"): command["attachments"] = [item["id"] for item in record["attachments"]]
    fingerprint = module.sha(memo.public_bytes(key))
    if delegated:
        command["delegation"] = {"schema": 1, "grant_id": fingerprint, "generation": "b" * 32}
        record["delegation_id"] = fingerprint
    command = memo.sign(command, key)
    record.update(author=fingerprint, public_key=command["public_key"], signature=command["signature"], signed_payload=memo.canonical(command).decode())
    module.validate_event(record, binding(), addressed=True)
    return record


class SenderControlsTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.path = Path(self.temp.name) / "inbox.sqlite"
        self.inbox = module.Inbox(self.path, binding())
        self.inbox.create(); self.inbox.add_consumer("planner"); self.inbox.add_consumer("auditor")
        self.record = signed_event()
        self.signer = self.record["author"]
        self.feed = Feed([self.record])

    def tearDown(self): self.temp.cleanup()

    def poll(self, *, resync=False):
        with patch.object(self.inbox, "_fetch", side_effect=self.feed.fetch):
            return self.inbox.resync() if resync else self.inbox.poll()

    def version(self):
        with closing(sqlite3.connect(self.path)) as db: return db.execute("PRAGMA user_version").fetchone()[0]

    def ledger(self):
        with closing(sqlite3.connect(self.path)) as db:
            return {table: db.execute("SELECT * FROM " + table).fetchall() for table in
                    ("binding", "checkpoint", "messages", "notifications", "consumers", "acknowledgements")}

    def enable(self): self.inbox.sender_controls_enable()

    def test_default_and_empty_migration_preserve_old_shapes_and_binding(self):
        self.poll(); before = self.ledger(); status = self.inbox.status(); pending = self.inbox.pending("planner")
        read = self.inbox.read("planner", pending[0]["notification_id"], True)
        with self.assertRaisesRegex(module.InboxError, "sender_controls_not_enabled"):
            self.inbox.sender_mute("planner", self.signer)
        self.assertEqual(self.version(), 1)
        self.assertEqual(self.inbox.sender_controls_enable(), {"enabled": True, "migrated": True})
        self.assertEqual(self.version(), 2)
        self.assertEqual(self.ledger(), before)
        self.assertEqual(self.inbox.sender_controls_enable(), {"enabled": True, "migrated": False})
        self.assertEqual(self.inbox.pending("planner"), pending)
        self.assertEqual(self.inbox.read("planner", pending[0]["notification_id"], True), read)
        self.assertEqual(self.inbox.status(), {**status, "local_schema": 2, "sender_mute_rules": 0})
        self.inbox.create()
        self.assertEqual(self.version(), 2)
        fresh = module.Inbox(Path(self.temp.name) / "fresh.sqlite", binding()); fresh.create()
        self.assertNotIn("local_schema", fresh.status())

    def test_two_consumers_exact_acknowledgements_and_body_denial(self):
        self.poll(); self.enable(); before = self.ledger(); notice = self.inbox.pending("planner")[0]
        with patch.object(self.inbox, "_fetch", side_effect=AssertionError("offline only")):
            self.assertEqual(self.inbox.sender_mute("planner", self.signer), {"muted": True, "changed": True})
            self.assertEqual(self.inbox.sender_mute("planner", self.signer), {"muted": True, "changed": False})
            self.assertEqual(self.ledger(), before)
            self.assertEqual(self.inbox.pending("planner"), [])
            self.assertEqual(self.inbox.pending("auditor"), [notice])
            self.assertEqual(self.inbox.pending("planner", include_muted=True), [{**notice, "muted": True}])
            self.assertEqual(self.inbox.read("planner", notice["notification_id"]), {**notice, "muted": True})
            with self.assertRaisesRegex(module.InboxError, "^sender_muted$"):
                self.inbox.read("planner", notice["notification_id"], True)
            self.assertEqual(self.inbox.read("auditor", notice["notification_id"], True)["snapshot"], self.record)
            self.inbox.ack("planner", notice["notification_id"], notice["snapshot_digest"])
            self.inbox.sender_unmute("planner", self.signer)
            self.assertEqual(self.inbox.pending("planner"), [])
            self.assertEqual(self.inbox.pending("auditor"), [notice])
        self.assertNotIn(self.signer, json.dumps(self.inbox.status()))
        self.assertNotIn(self.record["text"], json.dumps(self.inbox.status()))

    def test_old_successor_child_sibling_curator_and_anonymous_are_distinct(self):
        parent = self.record
        successor = signed_event("successor", 2, handle="same-handle")
        child = signed_event("child", 3, delegated=True)
        sibling = signed_event("sibling", 4, delegated=True)
        curator = signed_event("curator", 5, kind="imported", text="Original author: " + parent["author"])
        unsigned = event("unsigned", handle=parent["author"], text=parent["author"])
        self.feed.records = [parent, successor, child, sibling, curator, unsigned]
        self.poll(); self.enable()
        self.inbox.sender_mute("planner", parent["author"])
        self.assertEqual([item["message_id"] for item in self.inbox.pending("planner")], ["successor", "child", "sibling", "curator", "unsigned"])
        self.inbox.sender_mute("planner", child["author"])
        self.assertEqual([item["message_id"] for item in self.inbox.pending("planner")], ["successor", "sibling", "curator", "unsigned"])
        self.inbox.sender_unmute("planner", parent["author"])
        self.assertEqual(self.inbox.pending("planner")[0]["message_id"], "signed")
        self.assertEqual(self.inbox.sender_mutes("planner")["signers"], [child["author"]])

    def test_unreconciled_denial_precedes_mute_and_unmute(self):
        self.poll(); self.enable(); notice = self.inbox.pending("planner")[0]
        self.inbox.sender_mute("planner", self.signer)
        with closing(sqlite3.connect(self.path)) as db, db:
            db.execute("UPDATE checkpoint SET phase='corrections'")
        self.assertEqual(self.inbox.pending("planner", include_muted=True), [])
        for sensitive in (False, True):
            with self.assertRaisesRegex(module.InboxError, "superseded_or_unreconciled"):
                self.inbox.read("planner", notice["notification_id"], sensitive)
        self.inbox.sender_unmute("planner", self.signer)
        self.assertEqual(self.inbox.pending("planner"), [])

    def test_muted_claims_never_skip_signature_scope_or_capacity_validation(self):
        self.enable(); self.inbox.sender_mute("planner", self.signer)
        cases = [{**self.record, "signature": memo.b64(b"x" * 64)},
                 {**self.record, "delegation_id": self.signer},
                 {**self.record, "visibility": "private"},
                 {**self.record, "sha256": "f" * 64}]
        for index, record in enumerate(cases):
            with self.subTest(index=index):
                self.feed.records = [record]
                result = self.poll()
                self.assertNotEqual(result["phase"], "ready")
                self.assertTrue(result["last_error"])
                self.assertEqual(result["messages"], {})
        self.feed.records = [self.record]
        with patch.object(module, "BODY_BUDGET", 1):
            self.assertIn(self.poll()["last_error"], ("catalog_capacity", "notification_capacity"))
        self.assertEqual(self.inbox.status()["messages"], {})

    def test_corrections_ack_supersession_and_restart_keep_original_identity(self):
        attachment = {"id": "blob1", "room": "lobby", "filename": "untrusted.txt", "media_type": "text/plain", "sha256": "1" * 64,
                      "size": 10, "created_at": 1, "expires_at": 100, "deleted": False, "expired": False}
        self.record = signed_event(attachments=[attachment]); self.feed.records = [self.record]
        self.poll(); self.enable(); old = self.inbox.pending("planner")[0]
        self.inbox.sender_mute("planner", self.signer)
        changed = deepcopy(self.record); changed["attachments"][0]["deleted"] = True
        self.feed.changes.append(changed); self.poll()
        self.assertEqual(self.inbox.pending("planner"), [])
        audit = self.inbox.pending("planner", include_muted=True)[0]
        self.assertEqual(audit["kind"], "correction")
        self.assertNotEqual(audit["notification_id"], old["notification_id"])
        with self.assertRaisesRegex(module.InboxError, "superseded"):
            self.inbox.read("planner", old["notification_id"], True)
        with self.assertRaisesRegex(module.InboxError, "superseded"):
            self.inbox.ack("planner", old["notification_id"], old["snapshot_digest"])
        self.inbox = module.Inbox(self.path, binding())
        self.assertEqual(self.inbox.pending("planner"), [])
        self.inbox.sender_unmute("planner", self.signer)
        self.assertEqual(self.inbox.pending("planner"), [{key: value for key, value in audit.items() if key != "muted"}])
        self.inbox.sender_mute("planner", self.signer)
        self.feed.changes.append(self.record); self.poll()
        current = self.inbox.pending("planner", include_muted=True)[0]
        self.assertNotEqual(current["notification_id"], old["notification_id"])
        self.assertEqual(current["snapshot_digest"], old["snapshot_digest"])
        self.inbox.sender_unmute("planner", self.signer)
        self.assertEqual(len(self.inbox.pending("planner")), 1)

    def test_removal_and_unavailability_bypass_mute_and_never_resurrect(self):
        self.poll(); self.enable(); self.inbox.sender_mute("planner", self.signer)
        self.feed.changes.append(tombstone(self.record)); self.feed.records = [tombstone(self.record)]
        self.poll(); notice = self.inbox.pending("planner")[0]
        self.assertEqual(notice["kind"], "removal"); self.assertNotIn("muted", notice)
        self.assertIsNone(self.inbox.read("planner", notice["notification_id"], True)["snapshot"])
        self.inbox = module.Inbox(self.path, binding())
        self.feed.records = [self.record]; self.feed.changes.append(self.record)
        self.poll(); self.poll(resync=True); self.inbox.sender_unmute("planner", self.signer)
        self.assertEqual(self.inbox.pending("planner"), [notice])
        second = signed_event("missing", 1); self.feed.records.append(second); self.poll()
        self.inbox.sender_mute("planner", self.signer); self.feed.records = [self.record]
        self.poll(resync=True)
        notices = self.inbox.pending("planner")
        self.assertEqual([item["state"] for item in notices], ["tombstoned", "unavailable"])
        self.assertTrue(all("muted" not in item for item in notices))

    def test_first_seen_tombstone_without_signer_proof_always_bypasses(self):
        removed = tombstone(event("already-removed", author="c" * 64))
        self.feed.records = [removed, event("later-unavailable")]
        self.poll()
        notice = self.inbox.pending("planner")[0]
        sensitive = self.inbox.read("planner", notice["notification_id"], True)
        self.assertIsNone(sensitive["snapshot"])
        self.enable(); self.inbox.sender_mute("planner", "d" * 64)
        self.assertEqual(self.inbox.pending("planner")[0], notice)
        self.assertEqual(self.inbox.read("planner", notice["notification_id"]), notice)
        self.assertEqual(self.inbox.read("planner", notice["notification_id"], True), sensitive)
        self.feed.records = [removed]; self.poll(resync=True)
        notices = self.inbox.pending("planner", include_muted=True)
        self.assertEqual([item["state"] for item in notices], ["tombstoned", "unavailable"])
        for item in notices:
            self.assertNotIn("muted", item)
            self.assertIsNone(self.inbox.read("planner", item["notification_id"], True)["snapshot"])

    def test_prefix_filter_precedes_limit_and_queries_do_not_read_bodies(self):
        self.feed.records = [signed_event("prefix" + str(i)) for i in range(25)] + [signed_event("tail", 2)]
        self.poll(); self.enable(); self.inbox.sender_mute("planner", self.signer)
        real_database = self.inbox.database
        from contextlib import contextmanager
        reads = []
        @contextmanager
        def database(**kwargs):
            with real_database(**kwargs) as db:
                def authorizer(action, table, column, *_):
                    if action == sqlite3.SQLITE_READ:
                        reads.append((table, column))
                        if table == "messages" and column == "snapshot": return sqlite3.SQLITE_DENY
                    return sqlite3.SQLITE_OK
                db.set_authorizer(authorizer)
                yield db
        with patch.object(self.inbox, "database", database):
            self.assertEqual([item["message_id"] for item in self.inbox.pending("planner", 1)], ["tail"])
            audit = self.inbox.pending("planner", 1, include_muted=True)
            self.assertEqual(audit[0]["message_id"], "prefix0"); self.assertTrue(audit[0]["muted"])
            self.assertTrue(self.inbox.read("planner", audit[0]["notification_id"])["muted"])
            with self.assertRaisesRegex(module.InboxError, "sender_muted"):
                self.inbox.read("planner", audit[0]["notification_id"], True)
        self.assertNotIn(("messages", "snapshot"), reads)
        self.inbox.sender_unmute("planner", self.signer)
        with patch.object(self.inbox, "metadata_signer", side_effect=AssertionError("zero-rule fast path")):
            self.assertEqual(self.inbox.pending("planner", 1)[0]["message_id"], "prefix0")

    def test_policy_missing_corrupt_unknown_consumer_and_bad_metadata_fail_closed(self):
        self.poll(); self.enable(); self.inbox.sender_mute("planner", self.signer)
        before = self.ledger()
        with closing(sqlite3.connect(self.path)) as db, db: db.execute("ALTER TABLE sender_mutes RENAME TO broken_policy")
        for action in (self.inbox.status, lambda: self.inbox.pending("planner"), lambda: self.inbox.read("planner", 1, True), self.inbox.sender_controls_enable):
            with self.assertRaisesRegex(module.InboxError, "invalid_sender_policy"): action()
        with closing(sqlite3.connect(self.path)) as db, db:
            db.execute("DROP TABLE broken_policy"); db.execute(module.SENDER_SCHEMA)
            db.execute("INSERT INTO sender_mutes VALUES('unknown',?)", (self.signer,))
        with self.assertRaisesRegex(module.InboxError, "invalid_sender_policy"): self.inbox.status()
        with closing(sqlite3.connect(self.path)) as db, db:
            db.execute("DELETE FROM sender_mutes"); db.execute("INSERT INTO sender_mutes VALUES('planner',?)", (self.signer,))
        raw = before["messages"][0][2]
        for transform in (lambda value: {**value, "extra": "no"}, lambda value: {**value, "author": "f" * 64},
                          lambda value: {**value, "created_at": True}, lambda value: {**value, "room": "INVALID"}):
            with self.subTest(transform=transform):
                with closing(sqlite3.connect(self.path)) as db, db: db.execute("UPDATE events SET immutable=?", (module.encode(transform(module.strict_json(raw))),))
                with self.assertRaisesRegex(module.InboxError, "cache_integrity_error"): self.inbox.pending("planner")
                with self.assertRaisesRegex(module.InboxError, "cache_integrity_error"): self.inbox.read("planner", 1)
        with closing(sqlite3.connect(self.path)) as db, db:
            db.execute("UPDATE events SET immutable=zeroblob(?)", (module.MAX_EVENT + 1,))
        with self.assertRaisesRegex(module.InboxError, "cache_integrity_error"): self.inbox.pending("planner")
        with self.assertRaisesRegex(module.InboxError, "cache_integrity_error"): self.inbox.read("planner", 1)
        with self.inbox.database() as db:
            row = self.inbox.notification(db, "planner", 1, metadata_only=True)
            self.assertIsNone(row["immutable"], "oversized bytes must not cross SQLite/Python metadata boundary")

    def test_caps_accounting_full_unmute_and_strict_offline_arguments(self):
        self.enable()
        with self.inbox.database() as db: before = self.inbox.usage(db)
        self.inbox.sender_mute("planner", self.signer)
        with self.inbox.database() as db: self.assertEqual(self.inbox.usage(db), before + 256)
        with patch.object(module, "MAX_BYTES", 0):
            with self.assertRaisesRegex(module.InboxError, "catalog_capacity"): self.inbox.sender_mute("planner", "c" * 64)
            self.assertEqual(self.inbox.sender_unmute("planner", self.signer), {"muted": False, "changed": True})
            self.assertEqual(self.inbox.sender_unmute("planner", self.signer), {"muted": False, "changed": False})
        for bad in (True, 1, None, [], "A" * 64, "anonymous", "*", "a" * 63):
            with self.subTest(bad=bad), self.assertRaisesRegex(module.InboxError, "invalid_signer"): self.inbox.sender_mute("planner", bad)
        for bad in (None, 1, "false", [], {}):
            with self.subTest(bad=bad), self.assertRaisesRegex(module.InboxError, "invalid_include_muted"): self.inbox.pending("planner", include_muted=bad)
        with self.assertRaisesRegex(module.InboxError, "unknown_consumer"): self.inbox.sender_mute("missing", self.signer)
        for i in range(14): self.inbox.add_consumer("consumer" + str(i))
        with closing(sqlite3.connect(self.path)) as db, db:
            consumers = [row[0] for row in db.execute("SELECT id FROM consumers")]
            db.executemany("INSERT INTO sender_mutes VALUES(?,?)", [(consumer, f"{i:064x}") for consumer in consumers for i in range(128)])
        self.assertEqual(self.inbox.status()["sender_mute_rules"], 2048)
        self.assertEqual(self.inbox.sender_mute("planner", "0" * 64), {"muted": True, "changed": False})
        with self.assertRaisesRegex(module.InboxError, "sender_rule_limit"): self.inbox.sender_mute("planner", self.signer)
        self.inbox.sender_unmute("planner", "0" * 64)
        self.inbox.sender_mute("planner", self.signer)
        self.assertEqual(self.inbox.status()["sender_mute_rules"], 2048)

    def test_lock_permissions_cli_offline_and_redacted_outputs(self):
        config = Path(self.temp.name) / "binding.json"
        descriptor = os.open(config, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        with os.fdopen(descriptor, "wb") as stream: stream.write(module.encode(binding()))
        def cli(*args):
            out, err = io.StringIO(), io.StringIO()
            with redirect_stdout(out), redirect_stderr(err), patch.object(module.subprocess, "Popen", side_effect=AssertionError("no network")), patch.object(memo, "load_key", side_effect=AssertionError("no key")):
                code = module.main(["--db", str(self.path), "--binding", str(config), *args])
            return code, out.getvalue(), err.getvalue()
        for args in (("sender-controls-enable",), ("sender-mute", "planner", "--signer", self.signer), ("pending", "planner", "--include-muted"), ("status",)):
            code, out, err = cli(*args)
            self.assertEqual(code, 0, err); self.assertNotIn(self.signer, out); self.assertNotIn(self.record["text"], out)
        code, out, _ = cli("sender-mutes", "planner")
        self.assertEqual(json.loads(out)["signers"], [self.signer])
        with open(str(self.path) + ".lock", "a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(module.InboxError, "inbox_busy"): self.inbox.sender_unmute("planner", self.signer)
        self.path.chmod(0o644)
        with self.assertRaisesRegex(module.InboxError, "private_regular_file_required"): self.inbox.sender_mutes("planner")
        self.path.chmod(0o600)

    def test_hot_journal_restores_policy_without_network(self):
        self.poll(); self.enable()
        with closing(sqlite3.connect(self.path)) as db, db:
            db.executemany("INSERT INTO sender_mutes VALUES('planner',?)", [(f"{i:064x}",) for i in range(128)])
        before = self.ledger(); rules = self.inbox.sender_mutes("planner")
        code = "import os,sqlite3,sys;db=sqlite3.connect(sys.argv[1]);db.execute('PRAGMA cache_size=1');db.execute('BEGIN IMMEDIATE');db.execute(\"UPDATE sender_mutes SET signer='f'||substr(signer,2)\");os._exit(83)"
        child = subprocess.run([sys.executable, "-I", "-B", "-c", code, str(self.path)], timeout=10, capture_output=True)
        self.assertEqual(child.returncode, 83, child.stderr.decode())
        with Path(str(self.path) + "-journal").open("rb") as journal:
            self.assertEqual(journal.read(8), bytes.fromhex("d9d505f920a163d7"), "must be a genuinely hot journal before the Inbox's first reopen")
        with patch.object(self.inbox, "_fetch", side_effect=AssertionError("offline recovery")):
            self.assertEqual(self.inbox.sender_mutes("planner"), rules)
            self.assertEqual(self.ledger(), before)
            self.assertEqual(self.version(), 2)

    def test_migration_and_rule_crash_before_after_commit_and_hot_journal(self):
        self.poll(); before = self.ledger()
        code = r'''
import json,os,sqlite3,sys
sys.path.insert(0,sys.argv[1])
import swarmmemo_inbox as m
path,config,action,when,signer=sys.argv[2:]
real=m.sqlite3.connect
class Connection(sqlite3.Connection):
    def execute(self,sql,*args):
        result=super().execute(sql,*args)
        target=(action=='enable' and sql=='PRAGMA user_version=2') or (action=='mute' and sql.startswith('INSERT INTO sender_mutes')) or (action=='unmute' and sql.startswith('DELETE FROM sender_mutes'))
        if target:
            if when=='after': self.commit()
            os._exit(82 if when=='after' else 81)
        return result
def connect(*args,**kwargs): return real(*args,**kwargs,factory=Connection)
m.sqlite3.connect=connect
inbox=m.Inbox(path,json.loads(config))
if action=='enable': inbox.sender_controls_enable()
elif action=='mute': inbox.sender_mute('planner',signer)
else: inbox.sender_unmute('planner',signer)
'''
        def crash(action, when):
            child = subprocess.run([sys.executable, "-I", "-B", "-c", code, str(CLIENT_DIR), str(self.path), json.dumps(binding()), action, when, self.signer], timeout=10, capture_output=True)
            self.assertEqual(child.returncode, 82 if when == "after" else 81, child.stderr.decode())
        crash("enable", "before")
        self.assertEqual(self.version(), 1); self.assertEqual(self.ledger(), before)
        self.assertEqual(self.inbox.pending("planner")[0]["message_id"], "signed")
        crash("enable", "after")
        self.assertEqual(self.version(), 2); self.assertEqual(self.ledger(), before)
        for action, when, expected in (("mute", "before", []), ("mute", "after", [self.signer]), ("unmute", "before", [self.signer]), ("unmute", "after", [])):
            crash(action, when)
            self.assertEqual(self.inbox.sender_mutes("planner")["signers"], expected)
            self.assertEqual(self.ledger(), before)

    @unittest.skipUnless(os.environ.get("SWARMMEMO_OLD_INBOX_CLIENT_DIR"), "set an actual frozen schema1 client directory")
    def test_actual_old_reader_refuses_opt_in_schema2(self):
        old = Path(os.environ["SWARMMEMO_OLD_INBOX_CLIENT_DIR"]).resolve()
        self.assertNotEqual(old, CLIENT_DIR.resolve())
        code = "import json,sys;sys.path.insert(0,sys.argv[1]);import swarmmemo_inbox as m;print(json.dumps(m.Inbox(sys.argv[2],json.loads(sys.argv[3])).status()))"
        argv = [sys.executable, "-I", "-B", "-c", code, str(old), str(self.path), json.dumps(binding())]
        old_result = subprocess.run(argv, capture_output=True, timeout=10)
        self.assertEqual(old_result.returncode, 0, old_result.stderr.decode())
        self.assertEqual(json.loads(old_result.stdout), self.inbox.status())
        self.enable()
        old_result = subprocess.run(argv, capture_output=True, timeout=10)
        self.assertNotEqual(old_result.returncode, 0)
        self.assertIn(b"unsupported_inbox_version", old_result.stderr)
        self.assertEqual(self.version(), 2)


if __name__ == "__main__": unittest.main()
