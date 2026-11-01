"""C668: receiver create, rotate, delete, list and items from the CLI, without raw
service.call JSON; items --wait polls with a cursor and prints at most once."""
import contextlib
import io
import json
import os
import tempfile
import unittest
from unittest.mock import patch

import swarmmemo as memo

RID = "ab" * 16


class ReceiverCLITests(unittest.TestCase):
    def run_cli(self, *argv, answers=None, clock=None):
        """answers: one result dict per request, in order (default: an empty items page)."""
        key = memo.crypto()[0].from_private_bytes(bytes(32))
        sent, out, err, sleeps = [], io.StringIO(), io.StringIO(), []
        queue = list(answers or [])

        def fake(self, path, body=None):
            data = json.loads(body["data"])
            sent.append((body["operation"], body["target"], data))
            result = queue.pop(0) if queue else {"items": [], "next_after": data.get("args", {}).get("after", 0), "has_more": False}
            return {"ok": True, "data": {"service": "receiver", "method": data["method"], "result": result}}

        now = [0.0]

        def sleep(seconds):
            sleeps.append(seconds)
            now[0] += seconds
        with patch.object(memo, "load_key", return_value=key), patch.object(memo.Client, "_request", fake), \
                patch.object(memo.time, "sleep", sleep), patch.object(memo.time, "monotonic", lambda: now[0]), \
                contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            try:
                code = memo.main(["--key", "k.json", "receiver", *argv])
            except SystemExit as exc:
                code = exc.code
        return code, sent, out.getvalue(), err.getvalue(), sleeps

    def test_create_sends_the_spec_and_warns_the_url_is_shown_once(self):
        code, sent, out, err, _ = self.run_cli("create", "--label", "ci-results", "--no-screen", "--dedupe-header", "X-Event-Id",
                                               "--allow-from", "198.51.100.0/24", "--request-id", "r1",
                                               answers=[{"url": "https://swarmmemo.com/in/x/y", "receiver": {"id": RID}}])
        self.assertEqual(code, 0)
        op, target, data = sent[0]
        self.assertEqual((op, target, data["method"]), ("service.call", "receiver", "create"))
        self.assertEqual(data["args"], {"label": "ci-results", "screen": False, "dedupe_header": "X-Event-Id", "allow_from": ["198.51.100.0/24"]})
        self.assertIn("shown only this once", err)
        self.assertEqual(json.loads(out)["data"]["result"]["url"], "https://swarmmemo.com/in/x/y")

    def test_create_defaults_send_no_arguments(self):
        _, sent, _, _, _ = self.run_cli("create", answers=[{}])
        self.assertEqual(sent[0][2]["args"], {})

    def test_hmac_secret_comes_from_an_owner_only_file_never_argv(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "secret")
            with open(path, "w") as f:
                f.write("s3cret-value\n")
            os.chmod(path, 0o644)
            code, sent, _, err, _ = self.run_cli("create", "--hmac-secret-file", path, answers=[{}])
            self.assertNotEqual(code, 0)
            self.assertEqual(sent, [])
            self.assertNotIn("s3cret", err)
            os.chmod(path, 0o600)
            code, sent, _, _, _ = self.run_cli("create", "--hmac-secret-file", path, answers=[{}])
            self.assertEqual(code, 0)
            self.assertEqual(sent[0][2]["args"], {"hmac_secret": "s3cret-value"})

    def test_rotate_delete_and_list(self):
        for action in ("rotate", "delete"):
            code, sent, _, _, _ = self.run_cli(action, RID, answers=[{}])
            self.assertEqual(code, 0)
            self.assertEqual(sent, [("service.call", "receiver", sent[0][2])])
            self.assertEqual((sent[0][2]["method"], sent[0][2]["args"]), (action, {"id": RID}))
        code, sent, _, _, _ = self.run_cli("list", answers=[{"active": [], "ended": []}])
        self.assertEqual(code, 0)
        self.assertEqual(sent[0][:2], ("service.read", "receiver"))
        self.assertEqual((sent[0][2]["method"], sent[0][2].get("args", {})), ("list", {}))

    def test_items_reads_once_without_wait(self):
        code, sent, out, _, sleeps = self.run_cli("items", RID, "--after", "4", "--limit", "5", "--include-flagged")
        self.assertEqual(code, 0)
        self.assertEqual(len(sent), 1)
        self.assertEqual(sent[0][2]["args"], {"receiver": RID, "after": 4, "limit": 5, "include_flagged": True})
        self.assertEqual(json.loads(out)["data"]["result"]["items"], [])  # an empty page still prints without --wait
        self.assertEqual(sleeps, [])

    def test_items_wait_polls_until_an_item_and_prints_it_once(self):
        item = {"seq": 7, "id": "i1", "body": "result: 42"}
        empty = {"items": [], "next_after": 6, "has_more": False}
        code, sent, out, _, sleeps = self.run_cli("items", "--after", "6", "--wait", "30",
                                                  answers=[empty, empty, {"items": [item], "next_after": 7, "has_more": False}])
        self.assertEqual(code, 0)
        self.assertEqual([s[2]["args"] for s in sent], [{"after": 6}] * 3)
        self.assertEqual(sleeps, [1.0, 2.0])  # a bounded, growing pause
        self.assertEqual(json.loads(out)["data"]["result"]["items"], [item])

    def test_items_wait_with_nothing_exits_zero_silently_at_the_deadline(self):
        code, sent, out, _, sleeps = self.run_cli("items", RID, "--wait", "25")
        self.assertEqual((code, out), (0, ""))
        self.assertAlmostEqual(sum(sleeps), 25.0)
        self.assertLessEqual(max(sleeps), memo.RECEIVER_WAIT_POLL[1])
        self.assertEqual(len(sent), len(sleeps) + 1)

    def test_items_wait_bounds(self):
        code, sent, _, err, _ = self.run_cli("items", "--wait", "-1")
        self.assertEqual((code, sent), (1, []))
        self.assertIn("--wait", err)


if __name__ == "__main__":
    unittest.main()
