"""Opt-in works.list pagination follows opaque cursors without losing poll metadata."""
import contextlib
import io
import unittest
from unittest.mock import patch

import swarmmemo as memo


def page(rows, *, has_more, next_cursor=None, **metadata):
    result = {"ok": True, "data": {"works": rows, "has_more": has_more, **metadata}}
    if next_cursor is not None:
        result["next_cursor"] = next_cursor
    return result


class WorkAllPagesTests(unittest.TestCase):
    def test_library_collects_pages_deduplicates_and_keeps_first_poll_metadata(self):
        responses = [
            page([{"id": "a"}, {"id": "b"}], has_more=True, next_cursor="cursor-2", since="since-token", usdc_next=[{"id": "later"}]),
            page([{"id": "b"}, {"id": "c"}], has_more=True, next_cursor="cursor-3", next_poll_seconds=300),
            page([{"id": "d"}], has_more=False),
        ]
        client = memo.Client("https://example.org")
        with patch.object(client, "command", side_effect=responses) as command:
            got = client.works(kind="rewarded", limit=2, all_pages=True, max_pages=3)
        self.assertEqual(command.call_count, 3)
        self.assertEqual(command.call_args_list[0].kwargs, {"kind": "rewarded", "limit": 2})
        self.assertEqual(command.call_args_list[1].kwargs, {"kind": "rewarded", "limit": 2, "cursor": "cursor-2"})
        self.assertEqual(command.call_args_list[2].kwargs, {"kind": "rewarded", "limit": 2, "cursor": "cursor-3"})
        self.assertEqual([row["id"] for row in got["data"]["works"]], ["a", "b", "c", "d"])
        self.assertFalse(got["data"]["has_more"])
        self.assertNotIn("next_cursor", got)
        self.assertEqual(got["data"]["since"], "since-token")
        self.assertEqual(got["data"]["usdc_next"], [{"id": "later"}])

    def test_page_cap_keeps_last_cursor_and_since_cursor_starts_first_page(self):
        token = "since." + "a" * 32 + "." + "b" * 22
        responses = [
            page([{"id": "a"}], has_more=True, next_cursor="cursor-2", since=token),
            page([{"id": "b"}], has_more=True, next_cursor="cursor-3"),
        ]
        client = memo.Client("https://example.org")
        with patch.object(client, "command", side_effect=responses) as command:
            got = client.works(since_cursor=token, all_pages=True, max_pages=2)
        self.assertEqual(command.call_args_list[0].kwargs["cursor"], token)
        self.assertEqual(command.call_args_list[1].kwargs["cursor"], "cursor-2")
        self.assertEqual([row["id"] for row in got["data"]["works"]], ["a", "b"])
        self.assertTrue(got["data"]["has_more"])
        self.assertEqual(got["next_cursor"], "cursor-3")
        self.assertEqual(got["data"]["since"], token)

    def test_usdc_filter_runs_after_all_pages_are_combined(self):
        responses = [
            page([{"id": "low", "reward_usdc": {"amount": "0.05", "state": "promised"}}], has_more=True, next_cursor="cursor-2"),
            page([{"id": "high", "reward_usdc": {"amount": "0.20", "state": "promised"}}], has_more=False),
        ]
        client = memo.Client("https://example.org")
        with patch.object(client, "command", side_effect=responses):
            got = client.works(all_pages=True, max_pages=2, min_usdc="0.10")
        self.assertEqual([row["id"] for row in got["data"]["works"]], ["high"])
        self.assertFalse(got["data"]["has_more"])

    def test_later_page_error_is_not_returned_as_partial_success(self):
        responses = [page([{"id": "a"}], has_more=True, next_cursor="cursor-2"), {"ok": False, "error": "rate_limited"}]
        client = memo.Client("https://example.org")
        with patch.object(client, "command", side_effect=responses):
            self.assertEqual(client.works(all_pages=True, max_pages=2), {"ok": False, "error": "rate_limited"})

    def test_repeated_or_missing_cursor_is_refused(self):
        repeated = [page([{"id": "a"}], has_more=True, next_cursor="cursor-2"), page([{"id": "b"}], has_more=True, next_cursor="cursor-2")]
        client = memo.Client("https://example.org")
        with patch.object(client, "command", side_effect=repeated) as command:
            with self.assertRaisesRegex(memo.UsageError, "repeated next_cursor"):
                client.works(all_pages=True, max_pages=3)
        self.assertEqual(command.call_count, 2)
        with patch.object(client, "command", return_value=page([{"id": "a"}], has_more=True)) as command:
            with self.assertRaisesRegex(memo.UsageError, "no next_cursor"):
                client.works(all_pages=True, max_pages=2)
        command.assert_called_once()

    def test_invalid_max_pages_is_refused_before_request(self):
        client = memo.Client("https://example.org")
        with patch.object(client, "command") as command:
            for value in (0, 101, True, 1.5, "2"):
                with self.subTest(value=value), self.assertRaises(memo.UsageError):
                    client.works(all_pages=True, max_pages=value)
            command.assert_not_called()

    def test_cli_forwards_all_pages_options_and_requires_the_flag_for_cap(self):
        response = page([], has_more=False)
        with patch.object(memo.Client, "works", return_value=response) as works, contextlib.redirect_stdout(io.StringIO()):
            code = memo.main(["--anonymous", "work", "list", "--all-pages", "--max-pages", "2"])
        self.assertEqual(code, 0)
        self.assertTrue(works.call_args.kwargs["all_pages"])
        self.assertEqual(works.call_args.kwargs["max_pages"], 2)
        with patch.object(memo.Client, "command") as command, contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            code = memo.main(["--anonymous", "work", "list", "--max-pages", "2"])
        self.assertNotEqual(code, 0)
        command.assert_not_called()


if __name__ == "__main__":
    unittest.main()
