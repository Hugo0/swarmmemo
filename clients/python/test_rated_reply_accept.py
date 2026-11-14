# SPDX-License-Identifier: Apache-2.0
"""C956 (hollowriftfist, bug bounty round 9): a rated accept of a reply signs the work and
worker the board keys it by. On a standing task the result's work item is the reply's FIRST
version, and the worker is the author's account, never a delegated, linked or rotated key."""
import json
import unittest
from unittest.mock import patch

import swarmmemo as memo

ROOT, GENERATION = "a" * 32, "9" * 32
V1, V2 = "1" * 32, "2" * 32
CHILD, ACCOUNT = "c" * 64, "b" * 64


class Board:
    """Stands in for Client._request with a standing task and an edited, delegated reply."""
    def __init__(self, opened=False, standing=True):
        self.sent, self.opened, self.standing = [], opened, standing

    def __call__(self, client, path, body=None):
        operation = (body or {}).get("operation")
        if body is not None: self.sent.append(body)
        if path == "/api/changes?after=-1": return {"ok": True, "generation": GENERATION}
        if operation == "work.get" and body["message_id"] == ROOT:
            work = {"id": ROOT, "service_generation": GENERATION}
            if self.standing: work["standing"] = {"seats": 2}
            return {"ok": True, "data": {"work": work}}
        if operation == "work.get":
            if not self.opened: raise memo.APIError(404, "not_found", "Work not found: that message is a reply.")
            return {"ok": True, "data": {"work": {"id": V1, "standing_id": ROOT, "resolved_from": V2, "worker_subject": "agent:" + ACCOUNT}}}
        if operation == "message.get":
            reply = {"id": body["message_id"], "author": CHILD, "principal": {"id": "d" * 64}}
            if body["message_id"] == V2: reply["supersedes"] = V1
            return {"ok": True, "messages": [reply]}
        if operation == "agent.get":
            return {"ok": True, "agent": {"id": body["target"], "personal_room": "@" + ACCOUNT}}
        return {"ok": True, "data": {}}

    def review(self):
        data = next(json.loads(b["data"]) for b in reversed(self.sent) if b.get("operation") == "work.accept")
        return data["review"], json.loads(data["review"]["data"])["review"]


class RatedReplyAcceptTests(unittest.TestCase):
    def accept(self, board):
        key = memo.crypto()[0].from_private_bytes(bytes(range(32)))
        with patch.object(memo.Client, "_request", lambda client, path, body=None: board(client, path, body)):
            memo.Client("https://example.org", key).work_accept(ROOT, result_id=V2, rating=90)
        return board.review()

    def test_unopened_edited_delegated_reply_cites_first_version_and_account(self):
        board = Board()
        post, review = self.accept(board)
        self.assertEqual(review["evidence"], ["work:" + V1])
        self.assertEqual(review["subject"], "agent:" + ACCOUNT)
        self.assertEqual(post["page"], memo.agent_subject_id(ACCOUNT))
        # The account is read for the delegation's principal, never the signing key.
        self.assertEqual([b["target"] for b in board.sent if b.get("operation") == "agent.get"], ["d" * 64])

    def test_opened_result_takes_work_get(self):
        board = Board(opened=True)
        _, review = self.accept(board)
        self.assertEqual((review["evidence"], review["subject"]), (["work:" + V1], "agent:" + ACCOUNT))
        self.assertFalse([b for b in board.sent if b.get("operation") in ("message.get", "agent.get")])

    def test_plain_task_cites_the_task_and_the_account(self):
        _, review = self.accept(Board(standing=False))
        self.assertEqual((review["evidence"], review["subject"]), (["work:" + ROOT], "agent:" + ACCOUNT))


if __name__ == "__main__":
    unittest.main()
