"""C1012: erase_account_preview and erase_account send account.erase's two steps."""
import argparse
import json
import os
import tempfile
import time
import unittest
from unittest.mock import patch

import swarmmemo as memo


class AccountEraseTests(unittest.TestCase):
    def test_two_steps(self):
        calls = []

        def command(self, operation, **fields):
            calls.append((operation, json.loads(fields["data"])))
            return {"data": {"hold": "1.2.mac", "confirm_text": "erase abcdefgh"}}

        with patch.object(memo.Client, "command", command):
            client = memo.Client.__new__(memo.Client)
            client.erase_account_preview()
            client.erase_account("1.2.mac", "erase abcdefgh")
            client.erase_account("1.2.mac", "erase abcdefgh", recovery_code="smr1_x")
        self.assertEqual(calls, [
            ("account.erase", {"schema": 1, "step": "preview"}),
            ("account.erase", {"schema": 1, "step": "confirm", "hold": "1.2.mac", "confirm": "erase abcdefgh"}),
            ("account.erase", {"schema": 1, "step": "confirm", "hold": "1.2.mac", "confirm": "erase abcdefgh", "recovery_code": "smr1_x"}),
        ])


class FakeEraseClient:
    """A board whose previews hold for 24 hours after a recent key change: each preview's
    token opens a day after it, and a confirm before then is erase_waiting."""
    account = "abcdefgh" + "0" * 56

    def __init__(self, delay):
        self.delay, self.previews, self.confirms, self.now = delay, 0, [], int(time.time())

    def erase_account_preview(self):
        self.previews += 1
        opens = self.now + self.delay
        return {"data": {"step": "preview", "account": self.account, "hold": f"{opens}.{opens + 86400}.mac{self.previews}",
                         "confirm_after": opens, "hold_expires": opens + 86400, "confirm_text": "erase abcdefgh"}}

    def erase_account(self, hold, confirm, recovery_code=""):
        self.confirms.append(hold)
        if self.now < int(hold.split(".")[0]):
            raise memo.APIError(409, "erase_waiting", "confirm later")
        return {"data": {"step": "confirm", "receipt": {"account": self.account}}}


def erase_args(confirm="", hold=""):
    return argparse.Namespace(action="account-erase", confirm=confirm, hold=hold, recovery_code_file="")


class AccountEraseCLITests(unittest.TestCase):
    def setUp(self):
        home = tempfile.TemporaryDirectory()
        self.addCleanup(home.cleanup)
        patcher = patch.dict(os.environ, {"SWARMMEMO_HOME": home.name})
        patcher.start()
        self.addCleanup(patcher.stop)

    # C1040: under the recent-key hold, the confirm reuses the preview's saved hold, so
    # a day later it erases; it never previews again (a fresh hold would restart the wait).
    def test_held_preview_confirms_a_day_later(self):
        board = FakeEraseClient(delay=86400)
        with patch("sys.stderr"):
            preview = memo.run_account_erase(erase_args(), board)
        first = preview["data"]["hold"]
        self.assertEqual(memo.erase_holds()[board.account]["hold"], first)
        # Too early: the server says wait, and the saved hold stays.
        with self.assertRaises(memo.APIError) as early:
            memo.run_account_erase(erase_args("erase abcdefgh"), board)
        self.assertEqual(early.exception.code, "erase_waiting")
        # A second preview does not replace the hold that opens sooner.
        with patch("sys.stderr"):
            memo.run_account_erase(erase_args(), board)
        self.assertEqual(memo.erase_holds()[board.account]["hold"], first)
        board.now += 86400
        done = memo.run_account_erase(erase_args("erase abcdefgh"), board)
        self.assertEqual(done["data"]["step"], "confirm")
        self.assertEqual(board.confirms, [first, first])
        self.assertEqual(board.previews, 2)
        self.assertEqual(memo.erase_holds(), {})

    # A confirm with no saved hold previews once and returns that preview when its hold
    # opens later; with no wait it confirms at once with the fresh hold; --hold is sent as given.
    def test_confirm_without_saved_hold(self):
        board = FakeEraseClient(delay=86400)
        with patch("sys.stderr"):
            out = memo.run_account_erase(erase_args("erase abcdefgh"), board)
        self.assertEqual((out["data"]["step"], board.confirms), ("preview", []))
        now = FakeEraseClient(delay=0)
        memo.erase_hold_drop(now.account)
        self.assertEqual(memo.run_account_erase(erase_args("erase abcdefgh"), now)["data"]["step"], "confirm")
        self.assertEqual((now.previews, len(now.confirms)), (1, 1))
        given = FakeEraseClient(delay=0)
        memo.run_account_erase(erase_args("erase abcdefgh", hold="1.2.given"), given)
        self.assertEqual((given.previews, given.confirms), (0, ["1.2.given"]))


if __name__ == "__main__":
    unittest.main()
