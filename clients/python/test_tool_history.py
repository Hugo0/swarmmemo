"""C1003: tool_history reads service.calls; tool_history_all pages it to the end; cite sends service.cite."""
import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import swarmmemo as memo


class ToolHistoryTests(unittest.TestCase):
    def test_pages_merge_into_one_list(self):
        pages = [{"data": {"schema": 1, "calls": [{"call_id": "a"}], "next_cursor": "c1"}},
                 {"data": {"schema": 1, "calls": [{"call_id": "b"}]}}]
        calls = []

        def command(self, operation, **fields):
            calls.append((operation, fields))
            return pages[len(calls) - 1]

        with patch.object(memo.Client, "command", command):
            out = memo.Client.__new__(memo.Client).tool_history_all("fetch", limit=1, cursor="c0")
        self.assertEqual(calls, [("service.calls", {"target": "fetch", "cursor": "c0", "limit": 1}),
                                 ("service.calls", {"target": "fetch", "cursor": "c1", "limit": 1})])
        self.assertEqual([c["call_id"] for c in out["calls"]], ["a", "b"])
        self.assertNotIn("next_cursor", out)

    def test_since_and_cite_send_their_data(self):
        calls = []

        def command(self, operation, **fields):
            calls.append((operation, fields))
            return {"data": {}}

        with patch.object(memo.Client, "command", command):
            client = memo.Client.__new__(memo.Client)
            client.tool_history(since=1700000000)
            client.cite("0" * 32, public=False, request_id="r1")
        self.assertEqual(calls[0], ("service.calls", {"data": '{"schema":1,"since":1700000000}'}))
        self.assertEqual(calls[1][0], "service.cite")
        self.assertEqual(calls[1][1]["target"], "0" * 32)
        self.assertEqual(json.loads(calls[1][1]["data"]), {"schema": 1, "public": False})


    def test_paged_reads_refuse_save_request_before_sending(self):
        # C1043 (0f64e9bd): --save-request keeps one command in a NEW file (O_EXCL); --all sends
        # one per page, so page 2 failed after page 1 was read. Refused up front, CLI and library.
        calls = []

        def command(self, operation, **fields):
            calls.append(operation)
            return {"data": {"next_cursor": "c1", "has_more": True}}

        with tempfile.TemporaryDirectory() as tmp, patch.object(memo.Client, "command", command):
            saved = Path(tmp) / "req.json"
            for argv in (["history", "--all"], ["account-export", "--all"]):
                with self.assertRaises(SystemExit) as stop, contextlib.redirect_stderr(io.StringIO()) as err:
                    memo.main(["--save-request", str(saved)] + argv)
                self.assertEqual(stop.exception.code, 2)
                self.assertIn("--save-request saves one command", err.getvalue())
            client = memo.Client.__new__(memo.Client)
            client.save_request = saved
            with self.assertRaisesRegex(memo.UsageError, "history --all"):
                client.tool_history_all()
            with self.assertRaisesRegex(memo.UsageError, "account-export --all"):
                client.export_account_all()
            self.assertEqual(calls, [])
            self.assertFalse(saved.exists())


if __name__ == "__main__":
    unittest.main()
