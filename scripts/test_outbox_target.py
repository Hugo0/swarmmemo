"""Single-intent delivery: atomic selection, exact retry, local metadata only."""
import contextlib
import json
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import Mock, patch

CLIENT_DIR = Path(__file__).resolve().parents[1] / "clients/python"
sys.path.insert(0, str(CLIENT_DIR))
import swarmmemo as memo
import swarmmemo_outbox as outbox


def receipt(command):
    return {"ok": True, "receipt": {"id": "a" * 32, "sha256": outbox.sha(command["text"].encode()),
                                    "accepted_at": 1788566400, "cursor": "opaque-fixture", "duplicate": False}}


class TargetedOutboxTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / "queue.sqlite"
        self.key = memo.crypto()[0].generate()
        self.public = memo.b64(memo.public_bytes(self.key))
        self.queue = outbox.Outbox(self.path, "https://example.org", self.public)
        self.client = memo.Client("https://example.org", self.key, timeout=10)
        self.client.send = Mock(side_effect=receipt)

    def stage(self, name="first", **fields):
        return self.queue.enqueue(name, {"operation": "post", "room": "lobby", "text": "LOCAL_BODY_CANARY", **fields})["intent_sha256"]

    def deliver(self, name="first", digest=None, **kwargs):
        if digest is None: digest = self.queue.inspect(name)["intent_sha256"]
        return self.queue.deliver(self.client, name, digest, **kwargs)

    def test_target_only_and_ack_replay_never_flushes_next_intent(self):
        digest = self.stage(); self.stage("second")
        got = self.deliver(digest=digest)
        self.assertEqual(got["state"], "acknowledged")
        self.assertEqual(got["response"]["receipt"]["id"], "a" * 32)
        self.assertEqual(self.queue.inspect("second")["state"], "queued")
        with patch.object(self.client, "prepare", side_effect=AssertionError("no signing")):
            self.assertEqual(self.deliver(digest=digest), got)
        self.assertEqual(self.client.send.call_count, 1)
        self.assertNotIn("LOCAL_BODY_CANARY", json.dumps(got))
        self.assertNotIn("envelope", got)

    def test_wrong_id_digest_and_nonhead_are_side_effect_free(self):
        first = self.stage(); second = self.stage("second")
        before = self.queue.status()
        for name, digest, code in (("missing", first, "unknown_intent"), ("first", "f" * 64, "intent_digest_mismatch"),
                                    ("second", second, "outbox_not_head")):
            with self.subTest(code=code), self.assertRaisesRegex(outbox.OutboxError, code):
                self.deliver(name, digest)
        self.assertEqual(self.queue.status(), before)
        self.client.send.assert_not_called()
        self.deliver("first", first)
        with self.assertRaisesRegex(outbox.OutboxError, "intent_digest_mismatch"):
            self.deliver("first", "f" * 64)

    def test_invalid_selectors_never_create_queue(self):
        for name, digest, retry in ((None, "a" * 64, False), ("first", "A" * 64, False),
                                     ("first", None, False), ("first", "a" * 64, 1)):
            with self.assertRaises(outbox.OutboxError): self.queue.deliver(self.client, name, digest, retry_blocked=retry)
        with self.assertRaisesRegex(outbox.OutboxError, "unknown_intent"):
            self.queue.deliver(self.client, "first", "a" * 64)
        self.assertFalse(self.path.exists())

    def test_lost_response_restart_retains_exact_bytes_and_no_resigning(self):
        digest = self.stage(); self.stage("second")
        accepted = []
        def lost(command):
            with contextlib.closing(sqlite3.connect(self.path)) as db:
                raw, state = db.execute("SELECT envelope,state FROM queue WHERE id='first'").fetchone()
            self.assertEqual(raw, outbox.encoded(command)); self.assertEqual(state, "unresolved")
            accepted.append(raw)
            raise TimeoutError("SECRET_NETWORK_DETAILS")
        self.client.send.side_effect = lost
        first = self.deliver(digest=digest)
        self.assertEqual(first["state"], "unresolved")
        self.assertNotIn("SECRET", json.dumps(first))
        self.queue = outbox.Outbox(self.path, "https://example.org", self.public)
        def resend(command):
            self.assertEqual(outbox.encoded(command), accepted[0])
            r = receipt(command); r["receipt"]["duplicate"] = True
            return r
        self.client.send.side_effect = resend
        with patch.object(self.client, "prepare", side_effect=AssertionError("no replacement signature")):
            self.assertTrue(self.deliver(digest=digest)["response"]["receipt"]["duplicate"])
        self.assertEqual(self.queue.inspect("second")["attempts"], 0)

    def test_process_loss_after_persisted_send_is_resumable(self):
        # Anonymous mode avoids writing a key fixture. os._exit models a killed
        # sender after its persisted envelope has reached a fake accepting peer.
        self.queue = outbox.Outbox(self.path, "https://example.org", "")
        self.client = memo.Client("https://example.org", timeout=10)
        self.client.send = Mock(side_effect=receipt)
        digest = self.stage()
        code = '''
import os, sys
sys.path.insert(0, sys.argv[1])
import swarmmemo as m
import swarmmemo_outbox as o
q=o.Outbox(sys.argv[2], 'https://example.org', '')
c=m.Client('https://example.org', timeout=10)
c.send=lambda command: os._exit(79)
q.deliver(c, 'first', sys.argv[3])
'''
        result = subprocess.run([sys.executable, "-c", code, str(CLIENT_DIR), str(self.path), digest], capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 79, result.stderr)
        saved = self.queue.inspect("first", sensitive=True)
        self.assertEqual(saved["state"], "unresolved")
        with patch.object(self.client, "prepare", side_effect=AssertionError("must replay")):
            self.assertEqual(self.deliver(digest=digest)["state"], "acknowledged")
        self.assertEqual(self.client.send.call_args.args[0], saved["envelope"])

    def test_blocked_head_cannot_be_bypassed_and_explicit_retry_is_exact(self):
        first = self.stage(); second = self.stage("second")
        self.client.send.side_effect = memo.APIError(403, "delegation_inactive", "private detail")
        self.assertEqual(self.deliver(digest=first)["state"], "blocked")
        envelope = self.queue.inspect("first", sensitive=True)["envelope"]
        self.assertEqual(self.deliver(digest=first)["state"], "blocked")
        self.assertEqual(self.client.send.call_count, 1)
        with self.assertRaisesRegex(outbox.OutboxError, "outbox_not_head"):
            self.deliver("second", second, retry_blocked=True)
        self.client.send.side_effect = receipt
        with patch.object(self.client, "prepare", side_effect=AssertionError("must replay blocked envelope")):
            self.assertEqual(self.deliver(digest=first, retry_blocked=True)["state"], "acknowledged")
        self.assertEqual(self.client.send.call_args.args[0], envelope)

    def test_concurrent_delivery_selection_and_send_share_lock(self):
        first = self.stage(); second = self.stage("second")
        entered, finish = threading.Event(), threading.Event()
        results, errors = [], []
        def hold(command):
            entered.set()
            if not finish.wait(5): raise AssertionError("test lock deadline")
            return receipt(command)
        self.client.send.side_effect = hold
        def deliver():
            try: results.append(self.queue.deliver(self.client, "first", first))
            except BaseException as exc: errors.append(exc)
        thread = threading.Thread(target=deliver)
        thread.start()
        try:
            self.assertTrue(entered.wait(5))
            competitor = outbox.Outbox(self.path, "https://example.org", self.public)
            for name, digest in (("first", first), ("second", second)):
                with self.assertRaisesRegex(outbox.OutboxError, "outbox_busy"):
                    competitor.deliver(self.client, name, digest)
            with self.assertRaisesRegex(outbox.OutboxError, "outbox_busy"): competitor.flush(self.client)
        finally:
            finish.set(); thread.join(5)
        self.assertFalse(thread.is_alive()); self.assertEqual(errors, [])
        self.assertEqual(results[0]["state"], "acknowledged")
        self.assertEqual(self.client.send.call_count, 1)
        self.assertEqual(self.queue.inspect("second")["state"], "queued")

    def test_response_projection_and_invalid_saved_receipt_fail_closed(self):
        digest = self.stage()
        def extra(command):
            r = receipt(command); r["private_messages"] = "SECRET"
            r["receipt"]["instruction"] = "SECRET"
            return r
        self.client.send.side_effect = extra
        result = self.deliver(digest=digest)
        self.assertNotIn("SECRET", json.dumps(result))
        with self.queue.database(write=True) as db, db:
            bad = extra({"text": "different"})
            db.execute("UPDATE queue SET response=?", (outbox.encoded(bad),))
        with self.assertRaisesRegex(outbox.OutboxError, "invalid_acknowledgement"): self.deliver(digest=digest)
        self.assertEqual(self.client.send.call_count, 1)

    def test_invalid_new_metadata_is_unresolved_not_acknowledged(self):
        digest = self.stage()
        def invalid(command):
            r = receipt(command); r["receipt"]["duplicate"] = "yes"
            return r
        self.client.send.side_effect = invalid
        self.assertEqual(self.deliver(digest=digest)["state"], "unresolved")

    def test_retries_never_prepare_missing_or_tampered_envelopes(self):
        digest = self.stage()
        with self.queue.database(write=True) as db, db:
            db.execute("UPDATE queue SET state='unresolved'")
        with patch.object(self.client, "prepare", side_effect=AssertionError("must not recreate lost envelope")):
            with self.assertRaisesRegex(outbox.OutboxError, "outbox_integrity_error"):
                self.deliver(digest=digest)
        self.client.send.assert_not_called()

    def test_targeted_scope_and_legacy_rotation_barrier(self):
        key = memo.crypto()[0].generate(); public = memo.b64(memo.public_bytes(key))
        digest = self.queue.enqueue("rotate", {"operation": "agent.rotate", "target": public})["intent_sha256"]
        with self.assertRaisesRegex(outbox.OutboxError, "unsupported_targeted_mutation"):
            self.deliver("rotate", digest)
        self.stage("after")
        self.client.send.side_effect = lambda c: {"ok": True, "data": {"agent_id": outbox.sha(memo.unb64(public)), "predecessor": outbox.sha(memo.unb64(self.public))}}
        self.assertEqual(self.queue.flush(self.client, rotation_key=key)[0]["state"], "acknowledged")
        with self.assertRaisesRegex(outbox.OutboxError, "outbox_rotation_completed"): self.deliver("after")
        self.assertEqual(self.client.send.call_count, 1)

    def test_client_binding_is_checked_even_for_acknowledged_replay(self):
        digest = self.stage(); self.deliver(digest=digest)
        self.client.base_url = "https://elsewhere.example"
        with self.assertRaisesRegex(outbox.OutboxError, "client_binding_mismatch"): self.deliver(digest=digest)
        self.assertEqual(self.client.send.call_count, 1)

    def test_work_ack_return_and_delegated_historical_replay(self):
        context = {"schema": 1, "grant_id": outbox.sha(memo.unb64(self.public)), "generation": "b" * 32}
        self.queue = outbox.Outbox(self.path, "https://example.org", self.public, delegation=context)
        self.client = memo.DelegatedClient("https://example.org", self.key, grant_id=context["grant_id"],
                                          generation=context["generation"], room="lobby", operations=["work.claim"], timeout=10)
        intent = {"operation": "work.claim", "message_id": "a" * 32, "ttl": 120, "delegation": context,
                  "data": json.dumps({"schema": 1, "generation": context["generation"]})}
        digest = self.queue.enqueue("work", intent)["intent_sha256"]
        response = {"ok": True, "data": {"ack": {"work_id": "a" * 32, "state": "claimed", "fence": 1,
                    "generation": context["generation"], "service_id": memo.SERVICE, "accepted_at": 1788566400,
                    "deadline": 1788570000, "claim_expires_at": 1788566520}}}
        self.client.send = Mock(return_value=response)
        got = self.deliver("work", digest)
        self.assertEqual(got["response"], response)
        self.client.send.side_effect = memo.APIError(403, "delegation_inactive", "revoked")
        with patch.object(self.client, "prepare", side_effect=AssertionError("historical acknowledgement")):
            self.assertEqual(self.deliver("work", digest), got)
        self.assertEqual(self.client.send.call_count, 1)

    def test_targeted_renew_and_submit_preserve_fence_and_ack(self):
        for operation in ("work.renew", "work.submit"):
            intent = {"operation": operation, "message_id": "a" * 32, "amount": 7,
                      "data": json.dumps({"schema": 1, "generation": "b" * 32})}
            intent.update({"ttl": 120} if operation == "work.renew" else {"target": "c" * 32})
            digest = self.queue.enqueue(operation, intent)["intent_sha256"]
            ack = {"work_id": "a" * 32, "state": "claimed" if operation == "work.renew" else "submitted",
                   "fence": 7, "generation": "b" * 32, "service_id": memo.SERVICE,
                   "accepted_at": 1788566400, "deadline": 1788570000, "claim_expires_at": 1788566520}
            self.client.send.side_effect = lambda command: {"ok": True, "data": {"ack": ack}}
            got = self.deliver(operation, digest)
            self.assertEqual(got["response"]["data"]["ack"], ack)
            self.assertEqual(self.client.send.call_args.args[0]["amount"], 7)


if __name__ == "__main__": unittest.main()
