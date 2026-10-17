"""Regression for the public Python agent's unbounded cursor-file read.

This is an offline synthetic large local file. It never calls production
SwarmMemo, stores a signing key or touches a user's wallet or browser.
"""
import json
import tempfile
from pathlib import Path
import unittest

import swarmmemo as memo


class LocalOnlyClient:
    def __init__(self):
        self.calls = 0
        self.last_cursor = None

    def updates(self, agent=None, cursor="", limit=None, wait=None, counts=False, **data):
        self.calls += 1
        self.last_cursor = cursor
        return {"ok": True, "next_cursor": cursor}


class CursorBoundsTests(unittest.TestCase):
    def test_large_valid_cursor_json_is_rejected_before_any_network_call(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/"huge-cursor.json"
            path.write_text(json.dumps({"cursor":"x"*131072}), encoding="utf8")
            client=LocalOnlyClient()
            pages=memo.Client.follow_updates(client, cursor_file=str(path), pages=1)
            with self.assertRaisesRegex(ValueError,"cursor file exceeds 65536 bytes"):
                next(pages)
            self.assertEqual(client.calls,0,"Huge cursor reached updates.get before rejection")

    def test_normal_cursor_roundtrip_is_preserved(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/"cursor.json"
            path.write_text(json.dumps({"cursor":"opaque-abc"}), encoding="utf8")
            client=LocalOnlyClient()
            pages=memo.Client.follow_updates(client, cursor_file=str(path),pages=1)
            self.assertEqual(next(pages)["next_cursor"],"opaque-abc")
            self.assertEqual(client.calls,1)
            self.assertEqual(client.last_cursor,"opaque-abc")

    def test_missing_cursor_file_starts_at_empty_cursor(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/"not-created.json"
            client=LocalOnlyClient()
            pages=memo.Client.follow_updates(client, cursor_file=str(path),pages=1)
            next(pages)
            self.assertEqual(client.last_cursor,"")

    def test_bad_json_or_array_rejected_without_network_call(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/"cursor.json"
            for contents in ("[1]", "{notjson", '{"cursor": 3}'):
                path.write_text(contents, encoding="utf8")
                client=LocalOnlyClient()
                pages=memo.Client.follow_updates(client, cursor_file=str(path),pages=1)
                with self.assertRaises(ValueError):
                    next(pages)
                self.assertEqual(client.calls,0)


if __name__=="__main__":
    unittest.main()
