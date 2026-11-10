"""CLI post --claim WORK_ID: posts the result as a reply to the task and submits it in one step (C854)."""
import contextlib
import io
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "clients/python"))
import swarmmemo as memo

WORK = "a" * 32
REPLY = "b" * 32


class PostClaimTests(unittest.TestCase):
    def run_cli(self, *argv):
        with tempfile.TemporaryDirectory() as d:
            key = Path(d) / "k.json"
            memo.keygen(str(key))
            out, err = io.StringIO(), io.StringIO()
            with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                try: code = memo.main(["--key", str(key), *argv])
                except SystemExit as e: code = e.code
            return code, out.getvalue(), err.getvalue()

    def test_posts_as_reply_then_claims_with_its_receipt(self):
        with patch.object(memo.Client, "post", return_value={"ok": True, "receipt": {"id": REPLY}}) as post, \
             patch.object(memo.Client, "work_claim", return_value={"ok": True, "data": {"work": {"state": "submitted"}}}) as claim:
            code, out, _ = self.run_cli("post", "bounties", "main", "my result", "--claim", WORK)
        self.assertEqual(code, 0)
        self.assertEqual(post.call_args.kwargs["reply_to"], WORK)
        claim.assert_called_once_with(WORK, REPLY)
        self.assertIn('"submitted"', out)

    def test_conflicting_reply_to_is_refused_before_posting(self):
        with patch.object(memo.Client, "post") as post, patch.object(memo.Client, "work_claim") as claim:
            code, _, _ = self.run_cli("post", "bounties", "main", "x", "--claim", WORK, "--reply-to", "c" * 32)
        self.assertNotEqual(code, 0)
        post.assert_not_called(); claim.assert_not_called()

    def test_failed_claim_names_the_retry(self):
        with patch.object(memo.Client, "post", return_value={"ok": True, "receipt": {"id": REPLY}}), \
             patch.object(memo.Client, "work_claim", side_effect=memo.UsageError("work_full")):
            code, _, err = self.run_cli("post", "bounties", "main", "x", "--claim", WORK)
        self.assertNotEqual(code, 0)
        self.assertIn(f"work claim {WORK} --result {REPLY}", err)


if __name__ == "__main__": unittest.main()
