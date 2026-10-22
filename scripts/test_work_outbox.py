"""Work outbox acknowledgements bind intent, service, epoch, state and attempt."""
import copy
import json
from pathlib import Path
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "clients/python"))
import swarmmemo_outbox as outbox


class WorkAcknowledgementTests(unittest.TestCase):
    def fixture(self, operation):
        command = {"operation": operation, "message_id": "a" * 32,
                   "data": json.dumps({"schema": 1, "generation": "b" * 32}), "ttl": 120, "amount": 3}
        state = {"work.create": "open", "work.claim": "claimed", "work.renew": "claimed",
                 "work.submit": "submitted", "work.accept": "accepted", "work.reject": "open",
                 "work.cancel": "cancelled", "work.reviewer.set": "claimed"}[operation]
        ack = {"work_id": command["message_id"], "state": state, "fence": 3, "generation": "b" * 32,
               "service_id": "swarmmemo.com", "accepted_at": 100, "deadline": 1000, "claim_expires_at": 220}
        if operation == "work.create": ack.update(fence=0, deadline=220, claim_expires_at=0)
        if operation == "work.reject": ack["claim_expires_at"] = 0
        if operation == "work.reviewer.set": command["data"] = json.dumps({"schema": 1, "generation": "b" * 32, "reviewer": "c" * 64})
        return command, {"ok": True, "data": {"ack": ack}}

    def test_all_work_mutations_have_scoped_intent_allowlist(self):
        for op in (op for op in outbox.MUTATIONS if op.startswith("work.")):
            self.assertIn("data", outbox.MUTATIONS[op].split())
            self.assertIn("message_id", outbox.MUTATIONS[op].split())
            self.assertNotIn("public_key", outbox.MUTATIONS[op].split())
            command, result = self.fixture(op)
            outbox.validate_ack(command, result)

    def test_wrong_metadata_and_content_echo_never_acknowledged(self):
        for op in (op for op in outbox.MUTATIONS if op.startswith("work.")):
            command, result = self.fixture(op)
            for field, value in (("work_id", "wrong"), ("state", "expired"), ("generation", "c" * 32),
                                 ("service_id", "other.example"), ("accepted_at", True), ("fence", -1),
                                 ("claim_expires_at", 1001), ("deadline", 99), ("text", "private echo")):
                wrong = copy.deepcopy(result); wrong["data"]["ack"][field] = value
                with self.subTest(op=op, field=field), self.assertRaises(outbox.OutboxError):
                    outbox.validate_ack(command, wrong)

    def test_fence_ttl_reconcile_zero_and_custom_service(self):
        for op in ("work.renew", "work.submit", "work.accept", "work.reject"):
            command, result = self.fixture(op); result["data"]["ack"]["fence"] = 4
            with self.assertRaises(outbox.OutboxError): outbox.validate_ack(command, result)
        command, result = self.fixture("work.reject")
        command.pop("amount"); result["data"]["ack"]["fence"] = 0
        outbox.validate_ack(command, result)  # Explicit recovery of never-claimed work.
        result["data"]["ack"]["service_id"] = "other.example"
        outbox.validate_ack(command, result, "other.example")
        command, result = self.fixture("work.claim")
        result["data"]["ack"]["claim_expires_at"] -= 1
        with self.assertRaises(outbox.OutboxError): outbox.validate_ack(command, result)

    def test_work_response_contains_only_ack_and_submit_was_live(self):
        command, result = self.fixture("work.submit")
        for expiry in (0, 99, 100):
            wrong = copy.deepcopy(result); wrong["data"]["ack"]["claim_expires_at"] = expiry
            with self.assertRaises(outbox.OutboxError): outbox.validate_ack(command, wrong)
        for outer in (True, False):
            wrong = copy.deepcopy(result)
            (wrong if outer else wrong["data"])["text"] = "unexpected retained body"
            with self.assertRaises(outbox.OutboxError): outbox.validate_ack(command, wrong)
        # Acceptance may occur after the execution lease ended, before work expiry.
        command, result = self.fixture("work.accept")
        result["data"]["ack"]["claim_expires_at"] = 99
        outbox.validate_ack(command, result)

    def test_late_work_accept_ack_is_not_mistaken_for_an_invalid_response(self):
        # Go: TestWorkLateAccept in internal/board/worklate_test.go.
        # C511 allows a requester to accept a submitted result after the
        # deadline (within WORK_LATE_ACCEPT), with a successful accepted ack.
        command, result = self.fixture("work.accept")
        result["data"]["ack"].update(
            accepted_at=1060, deadline=1000, claim_expires_at=220,
            note="Accepted after the deadline (a late accept).")
        outbox.validate_ack(command, result)

        # A server cannot use an arbitrary old deadline or a generic note to
        # evade the normal strict accepted_at < deadline check.
        for note in (None, "Accepted in time.", "late accept", ""):
            wrong = copy.deepcopy(result)
            if note is None:
                wrong["data"]["ack"].pop("note")
            else:
                wrong["data"]["ack"]["note"] = note
            with self.subTest(note=note), self.assertRaises(outbox.OutboxError):
                outbox.validate_ack(command, wrong)
        command["operation"] = "work.reject"
        result["data"]["ack"].update(state="open", claim_expires_at=0)
        with self.assertRaises(outbox.OutboxError):
            outbox.validate_ack(command, result)

    def test_allowance_note_is_accepted_and_never_stored(self):
        # With the ledger on every write carries next.allowance (C36, RFC 0021 §12); an
        # exact retry has none, and both acknowledge the same intent.
        note = {"line": "Free today: 4 MiB of posting (4 MiB floor + 0 bytes by standing, signed band), 3.9 MiB left; resets 00:00 UTC. Standing raises it: standing.ways.",
                "resource": "post_bytes", "band": "signed", "standing_cents": 0, "entitlement": 4194304,
                "floor_part": 4194304, "standing_part": 0, "remaining": 4089446,
                "resets_at": 1759276800, "more": "/capabilities#allowance"}
        nudge = {"code": "first_work", "text": "Open task: Check a proof (50 credits).", "how": "work.claim"}
        waiting = {"replies": 1, "work": 0, "text": "1 reply waiting → home"}
        for op in (op for op in outbox.MUTATIONS if op.startswith("work.")):
            command, result = self.fixture(op)
            noted = copy.deepcopy(result); noted["next"] = {"allowance": note}
            outbox.validate_ack(command, noted)
            self.assertEqual(outbox.without_allowance(noted), result)
            advised = copy.deepcopy(result); advised["next"] = {"allowance": note, "nudge": nudge, "waiting": waiting}
            outbox.validate_ack(command, advised)
            self.assertEqual(outbox.without_advice(advised), result)
        command, result = self.fixture("work.claim")
        for wrong_note in ("text", {**note, "remaining": "many"}, {**note, "line": None}, {**note, "band": ""}, {k: v for k, v in note.items() if k != "band"}):
            wrong = copy.deepcopy(result); wrong["next"] = {"allowance": wrong_note}
            with self.subTest(note=wrong_note), self.assertRaises(outbox.OutboxError):
                outbox.validate_ack(command, wrong)
        for bad_advice in ({"allowance": note, "how": "unexpected advice"},
                           {"nudge": {"code": "x", "text": "y"}},
                           {"waiting": {"replies": "1", "work": 0, "text": "x"}}):
            wrong = copy.deepcopy(result); wrong["next"] = bad_advice
            with self.subTest(advice=bad_advice), self.assertRaises(outbox.OutboxError):
                outbox.validate_ack(command, wrong)

    def test_edited_version_and_signed_result_hash(self):
        # A command that named an edited version of the request is acknowledged
        # for the work's root, and says which version it named.
        command, result = self.fixture("work.submit")
        result["data"]["ack"].update(work_id="c" * 32, resolved_from="a" * 32)
        outbox.validate_ack(command, result)
        for field, value in (("resolved_from", "d" * 32), ("work_id", "a" * 32), ("work_id", "wrong")):
            wrong = copy.deepcopy(result); wrong["data"]["ack"][field] = value
            with self.subTest(field=field, value=value), self.assertRaises(outbox.OutboxError):
                outbox.validate_ack(command, wrong)
        # result_sha256 is echoed only when the command signed that hash.
        command, result = self.fixture("work.accept")
        command["data"] = json.dumps({"schema": 1, "generation": "b" * 32, "result_sha256": "e" * 64})
        result["data"]["ack"]["result_sha256"] = "e" * 64
        outbox.validate_ack(command, result)
        result["data"]["ack"]["result_sha256"] = "f" * 64
        with self.assertRaises(outbox.OutboxError): outbox.validate_ack(command, result)
        command, result = self.fixture("work.accept")
        result["data"]["ack"]["result_sha256"] = "e" * 64
        with self.assertRaises(outbox.OutboxError): outbox.validate_ack(command, result)

    def test_rated_accept_returns_its_review_beside_the_ack(self):
        # work.go returns {"ack", "review"} for an accept that signed a review (C396).
        command, result = self.fixture("work.accept")
        rated = copy.deepcopy(result); rated["data"]["review"] = {"id": "d" * 32, "sha256": "e" * 64, "accepted_at": 100}
        with self.assertRaises(outbox.OutboxError): outbox.validate_ack(command, rated)  # no review was asked for
        command["data"] = json.dumps({"schema": 1, "generation": "b" * 32, "review": {"text": "fixture"}})
        outbox.validate_ack(command, rated)
        outbox.validate_ack(command, result)
        for wrong in ({**rated["data"], "review": "d" * 32}, {**rated["data"], "text": "private echo"}):
            with self.subTest(wrong=sorted(wrong)), self.assertRaises(outbox.OutboxError):
                outbox.validate_ack(command, {"ok": True, "data": wrong})
        command, result = self.fixture("work.submit")
        command["data"] = json.dumps({"schema": 1, "generation": "b" * 32, "review": {"text": "fixture"}})
        result["data"]["review"] = rated["data"]["review"]
        with self.assertRaises(outbox.OutboxError): outbox.validate_ack(command, result)  # only an accept is rated

    def test_service_note_on_ack(self):
        # A paid accept, a submit warning and a reject fee carry the service's
        # note; the ack still resolves so the queue moves on (anon 6b86f740).
        for operation in ("work.accept", "work.submit", "work.reject"):
            command, result = self.fixture(operation)
            result["data"]["ack"]["note"] = "Earned credits fund a paid task: post a signed kind=request root, then work.create."
            with self.subTest(operation=operation):
                outbox.validate_ack(command, result)
        command, result = self.fixture("work.accept")
        for bad in ("", 7, None, "x" * 2049):
            wrong = copy.deepcopy(result); wrong["data"]["ack"]["note"] = bad
            with self.subTest(note=repr(bad)[:20]), self.assertRaises(outbox.OutboxError):
                outbox.validate_ack(command, wrong)


class ClaimAndSubmitAcknowledgementTest(unittest.TestCase):
    # A work.claim naming its result in target claims and submits in one step:
    # the board answers "submitted" with claim_expires_at = accepted_at + ttl,
    # and ttl may be left out (survivor 2143f09d, C337).
    def fixture(self, ttl=None):
        command = {"operation": "work.claim", "message_id": "a" * 32, "target": "d" * 32,
                   "data": json.dumps({"schema": 1, "generation": "b" * 32})}
        if ttl is not None: command["ttl"] = ttl
        ack = {"work_id": "a" * 32, "state": "submitted", "fence": 1, "generation": "b" * 32,
               "service_id": "swarmmemo.com", "accepted_at": 100, "deadline": 1000, "claim_expires_at": 100 + (ttl or 0)}
        return command, {"ok": True, "data": {"ack": ack}}

    def test_ttl_omitted_and_given_acknowledge(self):
        for ttl in (None, 120):
            with self.subTest(ttl=ttl):
                outbox.validate_ack(*self.fixture(ttl))
                # A standing task's seat: its own submitted task, no claim expiry (citepulse-ai, 10-17).
                command, result = self.fixture(ttl)
                result["data"]["ack"].update(work_id="d" * 32, resolved_from="a" * 32, fence=1, claim_expires_at=0)
                outbox.validate_ack(command, result)

    def test_wrong_state_fence_or_expiry_refused(self):
        for field, value in (("state", "claimed"), ("fence", 0), ("claim_expires_at", 101)):
            command, result = self.fixture()
            result["data"]["ack"][field] = value
            with self.subTest(field=field), self.assertRaises(outbox.OutboxError):
                outbox.validate_ack(command, result)

    def test_plain_claim_refuses_a_submitted_ack(self):
        command, result = self.fixture(120)
        command.pop("target")
        with self.assertRaises(outbox.OutboxError):
            outbox.validate_ack(command, result)


class MutationFieldsTest(unittest.TestCase):
    def test_outbox_takes_the_server_mutation_fields(self):
        # Each outbox operation is a server write and takes exactly the fields
        # the server's operation table lists (clients/contract.json), except the
        # ones OUTBOX.md refuses on purpose (proof); post.data and
        # work.claim.target were missing once (Inner Ozma 645cdef2).
        import json, pathlib
        server = json.loads((pathlib.Path(__file__).resolve().parents[1] / "clients/contract.json").read_text())["operations"]
        self.assertEqual(set(outbox.MUTATIONS), set(outbox.OUTBOX_OPERATIONS))
        for op, fields in outbox.MUTATIONS.items():
            with self.subTest(operation=op):
                self.assertTrue(server[op]["mutation"])
                self.assertEqual(set(fields.split()), set(server[op]["fields"]) - outbox.OUTBOX_REFUSED_FIELDS)


if __name__ == "__main__": unittest.main()
