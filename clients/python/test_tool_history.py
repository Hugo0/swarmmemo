"""C1003: tool_history reads service.calls; tool_history_all pages it to the end; cite sends service.cite."""
import json
import unittest
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


if __name__ == "__main__":
    unittest.main()
