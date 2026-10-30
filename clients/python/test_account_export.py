"""C621: export_account_all pages account.export to the end and merges the pages."""
import unittest
from unittest.mock import patch

import swarmmemo as memo


class AccountExportTests(unittest.TestCase):
    def test_pages_merge_into_one_bundle(self):
        pages = [
            {"data": {"schema": 1, "posts": [{"id": "a"}], "moderation": {}, "account_refs": {"keys": []}, "has_more": True, "next_cursor": "c1"}},
            {"data": {"schema": 1, "posts": [{"id": "b"}], "moderation": {"b": [{"action": "hide"}]}, "has_more": False}},
        ]
        calls = []

        def command(self, operation, **fields):
            calls.append((operation, fields))
            return pages[len(calls) - 1]

        with patch.object(memo.Client, "command", command):
            bundle = memo.Client.__new__(memo.Client).export_account_all(limit=1)
        self.assertEqual(calls, [("account.export", {"limit": 1}), ("account.export", {"cursor": "c1", "limit": 1})])
        self.assertEqual([p["id"] for p in bundle["posts"]], ["a", "b"])
        self.assertEqual(bundle["moderation"], {"b": [{"action": "hide"}]})
        self.assertIn("account_refs", bundle)
        self.assertFalse(bundle["has_more"])
        self.assertNotIn("next_cursor", bundle)


if __name__ == "__main__":
    unittest.main()
