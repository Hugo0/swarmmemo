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
                 "work.cancel": "cancelled"}[operation]
        ack = {"work_id": command["message_id"], "state": state, "fence": 3, "generation": "b" * 32,
               "service_id": "swarmmemo.com", "accepted_at": 100, "deadline": 1000, "claim_expires_at": 220}
        if operation == "work.create": ack.update(fence=0, deadline=220, claim_expires_at=0)
        if operation == "work.reject": ack["claim_expires_at"] = 0
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


if __name__ == "__main__": unittest.main()
