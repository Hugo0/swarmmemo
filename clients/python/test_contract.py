# SPDX-License-Identifier: Apache-2.0
"""The client validators against the server's message contract.

clients/message-fields.json is derived from the Go Message struct by
TestMessageFieldsGolden (internal/board), so a field the server adds fails
here until every strict allowlist knows it, not in an agent's inbox."""
import json
from pathlib import Path
import unittest

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

import swarmmemo as memo
import swarmmemo_inbox as inbox
import swarmmemo_private_transport as private
import test_private_transport as fixtures

CONTRACT = json.loads((Path(__file__).resolve().parents[1] / "message-fields.json").read_text())


class MessageContract(unittest.TestCase):
    def test_every_strict_allowlist_covers_the_server_fields(self):
        message = set(CONTRACT["message"])
        for name, fields in (("swarmmemo_inbox.EVENT_FIELDS", inbox.EVENT_FIELDS), ("swarmmemo_private_transport.EVENT_FIELDS", private.EVENT_FIELDS)):
            self.assertEqual(message - fields, set(), name)
        for name, fields in (("swarmmemo_inbox.ATTACHMENT_FIELDS", inbox.ATTACHMENT_FIELDS), ("swarmmemo_private_transport.ATTACHMENT_FIELDS", private.ATTACHMENT_FIELDS)):
            self.assertEqual(fields, set(CONTRACT["attachment"]), name)
        self.assertEqual(memo.FORWARDED_FIELDS, set(CONTRACT["forwarded"]))
        self.assertEqual(memo.QUALITY_FIELDS, set(CONTRACT["quality"]))
        self.assertEqual(memo.VOTE_FIELDS, set(CONTRACT["votes"]))
        self.assertEqual(memo.SCREEN_FIELDS, set(CONTRACT["screen"]))

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

    def test_private_read_metadata_and_markdown_are_accepted(self):
        key = Ed25519PrivateKey.generate()
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
