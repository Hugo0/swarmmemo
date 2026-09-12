"""Targeted CLI adapter: explicit authority, one durable intent, redacted output."""
import contextlib
import io
import json
from pathlib import Path
import sqlite3
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "clients/python"))
import swarmmemo as memo
import swarmmemo_outbox as outbox


class OutboxDeliveryCLITests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.path = self.directory / "queue.sqlite"
        self.key_path = self.directory / "child.json"
        self.public = memo.keygen(self.key_path)["public_key"]
        self.context = None
        self.queue = outbox.Outbox(self.path, "https://example.org", self.public)
        self.network = Mock(side_effect=self.acknowledgement)
        network_patch = patch.object(memo.Client, "_request", self.network)
        network_patch.start()
        self.addCleanup(network_patch.stop)

    @staticmethod
    def acknowledgement(path, command):
        if command["operation"] == "post":
            return {"ok": True, "receipt": {"id": "a" * 32,
                    "sha256": outbox.sha(command["text"].encode()), "accepted_at": 1788566400,
                    "duplicate": False, "private_extra": "REMOTE_SECRET_CANARY"},
                    "private_extra": "REMOTE_SECRET_CANARY"}
        return {"ok": True, "data": {"ack": {
            "work_id": command["message_id"], "state": "submitted" if command["operation"] == "work.submit" else "claimed",
            "fence": command.get("amount", 1), "generation": json.loads(command["data"])["generation"],
            "service_id": memo.SERVICE, "accepted_at": 1788566400,
            "deadline": 1788570000, "claim_expires_at": 1788566520}}}

    def arguments(self):
        result = ["--db", str(self.path), "--url", "https://example.org", "--public-key=" + self.public]
        if self.context is not None: result += ["--delegation-context", json.dumps(self.context)]
        return result

    def invoke(self, action, *, prefix=None):
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            try: code = outbox.main((self.arguments() if prefix is None else prefix) + action)
            except SystemExit as exc: code = exc.code
        for canary in ("LOCAL_BODY_CANARY", "REMOTE_SECRET_CANARY", "TRANSPORT_SECRET_CANARY"):
            self.assertNotIn(canary, stdout.getvalue() + stderr.getvalue())
        return code, stdout.getvalue(), stderr.getvalue()

    def stage(self, name="first", operation="post"):
        if operation == "post":
            intent = {"operation": "post", "room": "lobby", "text": "LOCAL_BODY_CANARY"}
            if self.context is not None: intent["visibility"] = "public"
        else:
            intent = {"operation": operation, "message_id": "a" * 32,
                      "data": json.dumps({"schema": 1, "generation": "b" * 32})}
            if operation in ("work.claim", "work.renew"): intent["ttl"] = 120
            if operation in ("work.renew", "work.submit"): intent["amount"] = 7
            if operation == "work.submit": intent["target"] = "c" * 32
        if self.context is not None: intent["delegation"] = self.context
        return self.queue.enqueue(name, intent)["intent_sha256"]

    def delivery(self, name="first", digest=None):
        result = ["deliver", "--id", name, "--intent-sha256", digest or self.queue.inspect(name)["intent_sha256"]]
        if self.public: result += ["--key", str(self.key_path)]
        if self.context is not None:
            result += ["--grant-room", "lobby"]
            for operation in ("post", "work.claim", "work.renew", "work.submit"):
                result += ["--grant-operation", operation]
        return result

    def delegated(self):
        self.context = {"schema": 1, "grant_id": outbox.sha(memo.unb64(self.public)), "generation": "b" * 32}
        self.queue = outbox.Outbox(self.path, "https://example.org", self.public, delegation=self.context)

    def test_target_ack_projection_and_historical_replay_leave_second_queued(self):
        digest = self.stage(); self.stage("second")
        code, stdout, stderr = self.invoke(self.delivery(digest=digest))
        result = json.loads(stdout)
        self.assertEqual((code, stderr, result["state"]), (0, "", "acknowledged"))
        self.assertEqual(set(result["response"]), {"ok", "receipt"})
        self.assertEqual(result["response"]["receipt"]["id"], "a" * 32)
        self.assertNotIn("envelope", result)
        with patch.object(memo.Client, "prepare", side_effect=AssertionError("historical replay must not sign")):
            self.assertEqual(self.invoke(self.delivery(digest=digest)), (code, stdout, stderr))
        self.assertEqual(self.network.call_count, 1)
        self.assertEqual(self.queue.inspect("second")["state"], "queued")
        self.assertEqual(self.queue.inspect("second")["attempts"], 0)

    def test_lost_response_retry_preserves_exact_signed_envelope_and_request_id(self):
        self.stage(); self.stage("second")
        accepted = []
        def lost(path, command):
            accepted.append(outbox.encoded(command))
            with contextlib.closing(sqlite3.connect(self.path)) as db:
                envelope, state = db.execute("SELECT envelope,state FROM queue WHERE id='first'").fetchone()
            self.assertEqual(outbox.encoded(command), envelope)
            self.assertEqual(state, "unresolved")
            raise TimeoutError("TRANSPORT_SECRET_CANARY")
        self.network.side_effect = lost
        code, stdout, stderr = self.invoke(self.delivery())
        self.assertEqual((code, stderr, json.loads(stdout)["state"]), (1, "", "unresolved"))
        self.assertNotIn("response", json.loads(stdout))
        def retry(path, command):
            self.assertEqual(outbox.encoded(command), accepted[0])
            self.assertEqual(command["request_id"], "first")
            response = self.acknowledgement(path, command)
            response["receipt"]["duplicate"] = True
            return response
        self.network.side_effect = retry
        with patch.object(memo.Client, "prepare", side_effect=AssertionError("no replacement signature")):
            code, stdout, _ = self.invoke(self.delivery())
        self.assertEqual(code, 0)
        self.assertTrue(json.loads(stdout)["response"]["receipt"]["duplicate"])
        self.assertEqual(self.queue.inspect("second")["attempts"], 0)

    def test_blocked_is_nonzero_and_repeated_delivery_never_retries(self):
        self.stage()
        self.network.side_effect = memo.APIError(403, "delegation_inactive", "TRANSPORT_SECRET_CANARY")
        first = self.invoke(self.delivery())
        self.assertEqual(first[0], 1)
        self.assertEqual(json.loads(first[1])["state"], "blocked")
        self.assertEqual(self.invoke(self.delivery()), first)
        self.assertEqual(self.network.call_count, 1)

    def test_wrong_selection_is_local_and_does_not_change_queue(self):
        first = self.stage(); second = self.stage("second")
        before = self.queue.status()
        cases = (("missing", first, "unknown_intent"), ("first", "f" * 64, "intent_digest_mismatch"),
                 ("second", second, "outbox_not_head"), ("first", "A" * 64, "invalid_intent_digest"),
                 ("invalid/id", first, "invalid_intent_id"))
        for name, digest, expected in cases:
            with self.subTest(expected=expected):
                code, stdout, stderr = self.invoke(self.delivery(name, digest))
                self.assertEqual((code, stdout, json.loads(stderr)), (1, "", {"error": expected}))
        self.assertEqual(self.queue.status(), before)
        self.network.assert_not_called()

    def test_missing_key_and_wrong_origin_key_service_are_local(self):
        self.stage()
        command = self.delivery()
        without_key = command[:-2]
        self.assertEqual(json.loads(self.invoke(without_key)[2])["error"], "client_binding_mismatch")
        other = self.directory / "other.json"
        memo.keygen(other)
        self.assertEqual(json.loads(self.invoke(command[:-1] + [str(other)])[2])["error"], "client_binding_mismatch")
        for option, value in (("--url", "https://different.example"), ("--service", "different.example")):
            prefix = self.arguments()
            if option in prefix: prefix[prefix.index(option) + 1] = value
            else: prefix += [option, value]
            code, stdout, stderr = self.invoke(command, prefix=prefix)
            self.assertEqual((code, stdout, json.loads(stderr)["error"]), (1, "", "outbox_binding_mismatch"))
        self.network.assert_not_called()
        self.assertEqual(self.queue.inspect("first")["state"], "queued")

    def test_delegated_four_operations_keep_context_and_exact_work_ack(self):
        self.delegated()
        for operation in ("post", "work.claim", "work.renew", "work.submit"):
            with self.subTest(operation=operation):
                self.stage(operation, operation)
                code, stdout, stderr = self.invoke(self.delivery(operation))
                result = json.loads(stdout)
                self.assertEqual((code, stderr, result["state"]), (0, "", "acknowledged"))
                path, command = self.network.call_args.args
                self.assertEqual(command["delegation"], self.context)
                self.assertTrue(memo.canonical(command).startswith(b'{"version":2,'))
                if operation != "post": self.assertEqual(result["response"], self.acknowledgement(path, command))

    def test_delegated_context_and_scope_never_fall_back(self):
        self.delegated(); self.stage()
        original = self.arguments()
        no_context = original[:-2]
        self.assertEqual(json.loads(self.invoke(self.delivery(), prefix=no_context)[2])["error"], "delegation_required")
        changed = dict(self.context, generation="c" * 32)
        self.assertEqual(self.invoke(self.delivery(), prefix=original[:-1] + [json.dumps(changed)])[0], 1)
        self.assertEqual(self.queue.inspect("first")["state"], "queued")
        command = self.delivery()
        command[command.index("--grant-room") + 1] = "different-room"
        code, stdout, _ = self.invoke(command)
        self.assertEqual((code, json.loads(stdout)["state"]), (1, "blocked"))
        self.assertEqual(json.loads(stdout)["last_error"], "delegation_scope_mismatch")
        self.network.assert_not_called()

    def test_anonymous_delivery_needs_no_key_or_crypto(self):
        self.public = ""
        self.queue = outbox.Outbox(self.path, "https://example.org", "")
        self.stage()
        with patch.object(memo, "crypto", side_effect=AssertionError("anonymous requires no cryptography")), \
             patch.object(memo, "load_key", side_effect=AssertionError("anonymous reads no keys")):
            code, stdout, _ = self.invoke(self.delivery())
        self.assertEqual((code, json.loads(stdout)["state"]), (0, "acknowledged"))
        self.assertNotIn("signature", self.network.call_args.args[1])

    def test_real_dash_prefixed_key_works_with_equals_argument(self):
        # Public, deterministic fixture seed; never an operational identity.
        seed = (33).to_bytes(32, "big")
        key = memo.crypto()[0].from_private_bytes(seed)
        self.public = memo.b64(memo.public_bytes(key))
        self.assertEqual(self.public, "-mLU3DYJV6Ej75jYvS8F5Zre6xyO33qzmpZHe4KZ0xg")
        self.key_path.write_text(json.dumps({"version": 1, "private_key": memo.b64(seed), "public_key": self.public}))
        self.queue = outbox.Outbox(self.path, "https://example.org", self.public)
        intent = self.directory / "dash-intent.json"
        intent.write_text(json.dumps({"operation": "post", "room": "lobby", "text": "LOCAL_BODY_CANARY"}))
        intent.chmod(0o600)
        with patch.object(memo, "load_key", side_effect=AssertionError("offline commands must not read keys")):
            self.assertEqual(self.invoke(["status"])[0], 0)
            self.assertEqual(self.invoke(["enqueue", "--id", "first", "--intent", str(intent)])[0], 0)
        self.network.assert_not_called()
        code, stdout, stderr = self.invoke(self.delivery())
        self.assertEqual((code, stderr, json.loads(stdout)["state"]), (0, "", "acknowledged"))
        command = self.network.call_args.args[1]
        self.assertEqual(command["public_key"], self.public)
        key.public_key().verify(memo.unb64(command["signature"]), memo.canonical(command))

    def test_default_help_status_and_enqueue_are_offline_without_key_reads(self):
        intent = self.directory / "intent.json"
        intent.write_text(json.dumps({"operation": "post", "text": "LOCAL_BODY_CANARY", "room": "lobby"}))
        intent.chmod(0o600)
        with patch.object(memo, "load_key", side_effect=AssertionError("no key reads")), \
             patch.object(memo.Client, "prepare", side_effect=AssertionError("no signing")):
            for command in ([], ["status"], ["--help"], ["deliver", "--help"],
                            ["enqueue", "--id", "first", "--intent", str(intent)], ["inspect", "--id", "first"]):
                with self.subTest(command=command): self.assertEqual(self.invoke(command)[0], 0)
        self.network.assert_not_called()
        self.assertIsNone(self.queue.inspect("first")["envelope_sha256"])

    def test_forbidden_options_and_required_selectors_reject_before_key_read(self):
        self.stage()
        with patch.object(memo, "load_key", side_effect=AssertionError("parser must reject before reading keys")):
            for extra in (["--retry-blocked"], ["--limit", "2"], ["--rotation-key", "unused"], ["--target-key", "unused"]):
                with self.subTest(extra=extra): self.assertEqual(self.invoke(self.delivery() + extra)[0], 2)
            self.assertEqual(self.invoke(["deliver", "--id", "first"])[0], 2)
            self.assertEqual(self.invoke(["deliver", "--intent-sha256", "a" * 64])[0], 2)
        self.network.assert_not_called()

    def test_non_targeted_operation_does_not_send(self):
        digest = self.queue.enqueue("register", {"operation": "agent.register", "handle": "local-fixture"})["intent_sha256"]
        code, stdout, stderr = self.invoke(self.delivery("register", digest))
        self.assertEqual((code, stdout, json.loads(stderr)), (1, "", {"error": "unsupported_targeted_mutation"}))
        self.network.assert_not_called()

    def test_invalid_ack_is_unresolved_and_key_errors_are_sanitized(self):
        self.stage()
        self.network.return_value = {"ok": True, "receipt": {"id": "a" * 32}}
        self.network.side_effect = None
        code, stdout, stderr = self.invoke(self.delivery())
        self.assertEqual((code, stderr, json.loads(stdout)["state"]), (1, "", "unresolved"))
        self.assertNotIn("response", json.loads(stdout))
        with patch.object(memo, "load_key", side_effect=ValueError("TRANSPORT_SECRET_CANARY")):
            code, stdout, stderr = self.invoke(self.delivery())
        self.assertEqual((code, stdout, json.loads(stderr)), (1, "", {"error": "outbox_operation_failed"}))


if __name__ == "__main__": unittest.main()
