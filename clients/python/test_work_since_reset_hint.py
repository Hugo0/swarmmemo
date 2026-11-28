"""work list --since: a valid but stale cursor (e.g. from before a server
restart) answers the full first page with data.since_reset true; the CLI prints
that as one hint line on stderr, beside the JSON, without changing the answer."""
import contextlib
import io
import json
import unittest
from unittest.mock import patch

import swarmmemo as memo

SINCE = "since." + "a" * 32 + "." + "b" * 22
HINT = ("hint since_reset: true; the --since cursor is from another server generation "
        "(for example, before a restart), so this is the full first page. "
        "Use data.since from this answer for the next --since.")


def page(**metadata):
    return {"ok": True, "data": {"works": [], "since": SINCE, "next_poll_seconds": 300, **metadata}}


def run_cli(argv, response):
    out, err = io.StringIO(), io.StringIO()
    with patch.object(memo.Client, "command", return_value=response) as command, \
            contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
        code = memo.main(argv)
    return code, command, json.loads(out.getvalue()), err.getvalue()


class WorkSinceResetHintTests(unittest.TestCase):
    def test_reset_prints_one_hint_line_beside_the_unchanged_json(self):
        response = page(since_reset=True)
        code, command, got, err = run_cli(
            ["--anonymous", "work", "list", "--kind", "rewarded", "--since", SINCE], response)
        self.assertEqual(code, 0)
        self.assertEqual(command.call_args.args, ("works.list",))
        self.assertEqual(command.call_args.kwargs, {"kind": "rewarded", "cursor": SINCE})
        self.assertEqual(got, response)  # the hint is beside the answer, never in it
        self.assertEqual(err.splitlines(), [HINT])

    def test_same_generation_full_page_prints_no_hint(self):
        # A same-generation cursor that is not the current page also gets the
        # full first page, but the board does not set since_reset (C540).
        response = page()
        code, _, got, err = run_cli(
            ["--anonymous", "work", "list", "--kind", "rewarded", "--since", SINCE], response)
        self.assertEqual(code, 0)
        self.assertEqual(got, response)
        self.assertEqual(err, "")

    def test_only_true_and_only_since(self):
        # The hint is scoped to the --since poll and to a literal true answer.
        for argv, response in (
                (["--anonymous", "work", "list", "--since", SINCE], page(since_reset="true")),
                (["--anonymous", "work", "list", "--cursor", SINCE], page(since_reset=True)),
                (["--anonymous", "work", "list"], page(since_reset=True))):
            with self.subTest(argv=argv, data=response["data"]):
                _, _, _, err = run_cli(argv, response)
                self.assertEqual(err, "")


if __name__ == "__main__":
    unittest.main()
