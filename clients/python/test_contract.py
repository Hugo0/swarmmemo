# SPDX-License-Identifier: Apache-2.0
"""The client validators against the server's contract.

clients/contract.json is generated from the Go code that enforces each fact
(go generate ./internal/clientcontract), and so is the CONTRACT block in
swarmmemo.py. The block must equal the file, and every allowlist the clients
check with must equal the server's set, so a field or enum value the server
adds fails here, not in an agent's inbox."""
import json
from pathlib import Path
import unittest


import swarmmemo as memo
import swarmmemo_inbox as inbox
import swarmmemo_outbox as outbox
import swarmmemo_private_transport as private
import test_private_transport as fixtures

CONTRACT = json.loads((Path(__file__).resolve().parents[1] / "contract.json").read_text())


def fields(name):
    return set(CONTRACT["structs"][name]["required"]) | set(CONTRACT["structs"][name]["optional"])


class MessageContract(unittest.TestCase):
    def test_generated_block_is_the_contract(self):
        # Two-way: the client's embedded copy is exactly clients/contract.json.
        self.assertEqual(memo.CONTRACT, CONTRACT)

    def test_every_strict_allowlist_is_the_server_set(self):
        message = fields("message")
        for name, allow in (("swarmmemo_inbox.EVENT_FIELDS", inbox.EVENT_FIELDS), ("swarmmemo_private_transport.EVENT_FIELDS", private.EVENT_FIELDS)):
            self.assertEqual(allow, message, name)
        for name, allow in (("swarmmemo_inbox.ATTACHMENT_FIELDS", inbox.ATTACHMENT_FIELDS), ("swarmmemo_private_transport.ATTACHMENT_FIELDS", private.ATTACHMENT_FIELDS)):
            self.assertEqual(allow, fields("attachment"), name)
        self.assertEqual(memo.FORWARDED_FIELDS, fields("forwarded"))
        self.assertEqual(memo.QUALITY_FIELDS, fields("quality"))
        self.assertEqual(memo.VOTE_FIELDS, fields("votes"))
        self.assertEqual(memo.SCREEN_FIELDS, fields("screen"))
        post = set(CONTRACT["operations"]["post"]["fields"]) | set(CONTRACT["command"]["envelope"])
        self.assertEqual(inbox.SIGNED_POST_FIELDS, post - {"signature"})
        self.assertEqual(private.POST_FIELDS, post - {"signature", "delegation"})
        self.assertEqual(memo.FIELDS, CONTRACT["command"]["fields"])
        self.assertEqual(memo.DELEGATED_OPERATIONS, {name for name, op in CONTRACT["operations"].items() if op["delegable"]})
        for op, allow in outbox.MUTATIONS.items():
            self.assertEqual(set(allow.split()), set(CONTRACT["operations"][op]["fields"]) - outbox.OUTBOX_REFUSED_FIELDS, op)

    def test_every_enum_is_the_server_list(self):
        enums = CONTRACT["enums"]
        for name, have, want in (("WORK_STATES", memo.WORK_STATES, set(enums["work_states"])),
                                 ("WORK_LIST_KINDS", memo.WORK_LIST_KINDS, enums["work_list_kinds"]),
                                 ("WORK_CREDIT_STATES", memo.WORK_CREDIT_STATES, set(enums["work_credit_states"])),
                                 ("WORK_USDC_STATES", memo.WORK_USDC_STATES, set(enums["work_usdc_states"])),
                                 ("WORK_REWARD_STATES", memo.WORK_REWARD_STATES, set(enums["work_reward_states"])),
                                 ("WORK_RESULT_STATES", memo.WORK_RESULT_STATES, set(enums["work_result_states"])),
                                 ("WORK_ELIGIBILITIES", memo.WORK_ELIGIBILITIES, set(enums["work_eligibilities"])),
                                 ("REVIEW_RELATIONSHIPS", memo.REVIEW_RELATIONSHIPS, enums["review_relationships"]),
                                 ("SCREEN_CATEGORIES", list(memo.SCREEN_CATEGORIES), enums["screen_categories"])):
            self.assertEqual(have, want, name)
        choices = {}
        parser = memo.build_parser()
        for action in parser._subparsers._group_actions[0].choices["review"]._actions:
            choices[action.dest] = action.choices
        self.assertEqual(list(choices["verdict"]), enums["review_verdicts"])
        self.assertEqual(list(choices["relationship"]), enums["review_relationships"])

    def test_public_read_metadata_is_accepted_and_checked(self):
        text = "a hosted post"
        event = {"id": "event1", "sequence": 1, "room": "lobby", "page": "main", "text": text, "kind": "note",
                 "author": "anonymous", "created_at": 1788566400, "sha256": inbox.sha(text.encode()),
                 "hidden": False, "type": "message", "visibility": "public", "archive_eligible": True,
                 "author_handle": "someone", "image_url": "https://example.org/e/event1.png", "custody": "hosted",
                 "quality": {"score": 0.5, "classifier_version": "q1"}, "votes": {"up": 2, "down": 1, "score": 1},
                 "screen": {"state": "pass", "withheld": False, "categories": {"spam": 0.1}}}
        binding = {"room": "", "service_id": "swarmmemo.com"}
        self.assertIs(inbox.validate_event(event, binding), event)
        for field, bad in (("custody", "kept"), ("sealed", True), ("screen", {"state": "pass"}), ("quality", {"score": 2, "classifier_version": ""})):
            with self.assertRaises(inbox.InboxError, msg=field):
                inbox.validate_event({**event, field: bad}, binding)
        # Who wrote it: an unsigned post may carry its daily network tag, never
        # a name source or a generated nickname (C67, C68).
        self.assertIs(inbox.validate_event({**event, "anon_tag": "d092"}, binding)["anon_tag"], "d092")
        for extra in ({"anon_tag": "D092"}, {"anon_tag": "d0929a1"}, {"nickname": "sable-bellows"}, {"display_name_source": "handle"}):
            with self.assertRaises(inbox.InboxError, msg=extra):
                inbox.validate_event({**event, **extra}, binding)
        signed = {"public_key": "k", "author": "a" * 64}
        memo.check_read_metadata({**signed, "nickname": "sable-bellows", "display_name_source": "generated"})
        memo.check_read_metadata({**signed, "display_name_source": "handle"})
        for extra in ({"nickname": "sable-bellows", "display_name_source": "handle"}, {"display_name_source": "generated"}, {"anon_tag": "d092"}, {"nickname": "<b>x</b>"}):
            with self.assertRaises(ValueError, msg=extra):
                memo.check_read_metadata({**signed, **extra})
        # A ranked entry's folded copies (C263).
        memo.check_read_metadata({**signed, "copies": {"count": 5, "ids": ["a1", "b2"]}})
        for bad in ({"count": 0, "ids": []}, {"count": 1, "ids": ["a", "b"]}, {"count": 2}, {"count": 2, "ids": [3]}, "5"):
            with self.assertRaises(ValueError, msg=bad):
                memo.check_read_metadata({**signed, "copies": bad})
            with self.assertRaises(ValueError, msg=bad):
                memo.check_read_metadata({**signed, "similar": bad})
        # A burst of one key's similar posts on a page (C601).
        memo.check_read_metadata({**signed, "similar": {"count": 2, "ids": ["a1", "b2"]}})
        memo.check_read_metadata({**signed, "similar_to": "a1"})
        for bad in ({"similar_to": 3}, {"similar_to": ""}, {"similar_to": "a1", "similar": {"count": 1, "ids": ["b2"]}}):
            with self.assertRaises(ValueError, msg=bad):
                memo.check_read_metadata({**signed, **bad})

    def test_work_marks_cover_the_server_fields(self):
        # C264: a message's work mark names exactly the Go MessageWork fields.
        self.assertEqual(memo.WORK_ROOT_FIELDS | memo.WORK_ROOT_OPTIONAL | memo.WORK_RESULT_FIELDS, fields("message_work"))
        work_id = "d" * 32
        mark = {"id": work_id, "title": "Find a bug", "state": "open", "deadline": 1788566400, "eligibility": "open", "claimable": True,
                "url": "/work/" + work_id, "simulated": True, "reward": {"amount": 5, "unit": "credit", "state": "held"},
                "reward_usdc": {"amount": "0.20", "state": "promised"}, "reward_state": "held", "reward_note": "Paid by the poster on Polygon",
                "reviewer": {"id": "a" * 64, "public_key": "k", "handle": "reviewer"},
                "standing": {"seats": 3, "tiers": [{"name": "bug", "credits": 150, "usdc": "0.20"}, {"name": "bug+fix", "credits": 300}]}}
        self.assertEqual(set(mark), memo.WORK_ROOT_FIELDS | memo.WORK_ROOT_OPTIONAL)
        memo.check_work({"type": "message", "work": mark})
        # C434: a standing task's terms are seats and 1 to 3 tiers.
        for bad in ({"seats": 0, "tiers": [{"name": "bug", "credits": 1}]}, {"seats": 3, "tiers": []}, {"seats": 3},
                    {"seats": 3, "tiers": [{"name": "bug", "credits": 0}]}, {"seats": 3, "tiers": [{"credits": 1}]}):
            with self.assertRaises(ValueError, msg=bad):
                memo.check_work({"type": "message", "work": {**mark, "standing": bad}})
        memo.check_work({"type": "message", "work": {k: v for k, v in mark.items() if k != "reward"}})
        # An open standing task with every seat taken is open and not claimable (exactlane 34209510).
        memo.check_work({"type": "message", "work": {**mark, "claimable": False}})
        for bad in ({"state": "accepted", "claimable": True}, {"claimable": "yes"}):
            with self.assertRaises(ValueError, msg=bad):
                memo.check_work({"type": "message", "work": {**mark, **bad}})
        # Shaped like #bounties root 6ea0a607: a paid credit-only reward.
        paid_id = "6ea0a607bc8d67295573129bc80ca4d3"
        memo.check_work({"type": "message", "work": {"id": paid_id, "title": "Bounty", "state": "accepted", "deadline": 1788566400, "eligibility": "open",
                                                     "claimable": False, "url": "/work/" + paid_id, "reward": {"amount": 6000, "unit": "credit"}, "reward_state": "paid"}})
        result = {"result_of": work_id, "title": "Find a bug", "state": "accepted", "url": "/work/" + work_id}
        memo.check_work({"type": "message", "reply_to": work_id, "work": result})
        # A standing task's result, then its edits: result_of stays the first version's id (f5654d25).
        first, standing, v2, v3 = "1" * 32, "2" * 32, "3" * 32, "4" * 32
        own = {"result_of": first, "title": "Result", "state": "submitted", "url": "/work/" + first}
        memo.check_work({"type": "message", "id": first, "reply_to": standing, "work": own})
        memo.check_work({"type": "message", "id": v2, "reply_to": standing, "supersedes": first, "work": own})
        memo.check_work({"type": "message", "id": v3, "reply_to": standing, "supersedes": v2, "work": own})
        with self.assertRaises(ValueError):  # not an edit: an unrelated work id is still refused
            memo.check_work({"type": "message", "id": v2, "reply_to": standing, "work": own})
        for bad in ({"reward_state": "promised"}, {"reward_state": ""}, {"reward_usdc": {"amount": 0.2, "state": "promised"}},
                    {"reward_usdc": {"amount": "1e3", "state": "promised"}}, {"reward_usdc": {"amount": "0.20", "state": "held"}},
                    {"reward_usdc": {"amount": "0.20"}}, {"reward_usdc": {"amount": "0.20", "state": "paid", "extra": 1}},
                    {"reward": {"amount": 5, "unit": "credit", "state": "void"}}, {"reward": {"amount": 5, "unit": "credit", "bonus": 1}},
                    {"reward_note": ""}, {"reward_note": " padded"}, {"reward_note": "two\nlines"}, {"reward_note": "x" * 81},
                    {"reward_note": "rtl‮override"}, {"reward_note": 5}):
            with self.assertRaises(ValueError, msg=bad):
                memo.check_work({"type": "message", "work": {**mark, **bad}})
        with self.assertRaises(ValueError):
            memo.check_work({"type": "message", "reply_to": work_id, "work": {**result, "reward_state": "held"}})

    def test_private_read_metadata_and_markdown_are_accepted(self):
        key = memo.crypto()[0].generate()
        text = "**private** markdown"
        command = memo.sign({"operation": "post", "room": "secret-a", "text": text, "data": '{"format":"markdown","schema":1}'}, key)
        event = {**fixtures.event(key, text=text), "signed_payload": memo.canonical(command).decode(), "signature": command["signature"],
                 "format": "markdown", "author_handle": "member", "custody": "hosted", "screen": {"state": "pass", "withheld": False}}
        binding = fixtures.binding(key)
        self.assertIs(private.validate_private_event(event, binding), event)
        with self.assertRaises(private.PrivateInboxError):
            private.validate_private_event({**event, "format": ""}, binding)
        with self.assertRaises(private.PrivateInboxError):
            private.validate_private_event({**event, "delegation_id": event["author"]}, binding)


if __name__ == "__main__":
    unittest.main()
