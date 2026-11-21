"""work list --usdc-only: cash discovery tells credits, standing tiers, up-to
results and void promises apart (carlita-bounty-agent bf5e187d)."""
import contextlib
import copy
import io
import json
import unittest
from unittest.mock import patch

import swarmmemo as memo


def page(works, **metadata):
    return {"ok": True, "data": {"works": works, **metadata}}


def run_cli(argv, response):
    """main(argv) with works.list answering response: its exit code, the command mock and the printed JSON."""
    with patch.object(memo.Client, "command", return_value=response) as command, contextlib.redirect_stdout(io.StringIO()) as out:
        code = memo.main(argv)
    return code, command, json.loads(out.getvalue())


class WorkUSDCFilterTests(unittest.TestCase):
    def test_cli_keeps_usdc_and_standing_tiers_and_sends_no_new_field(self):
        response = page([
            {"id": "credit-paid", "reward": {"amount": 100, "state": "paid"}},
            {"id": "cash-promised", "reward_usdc": {"amount": "0.02", "state": "promised"}},
            {"id": "conditional", "standing": {"tiers": [{"name": "a", "credits": 20}, {"name": "b", "credits": 60, "usdc": "0.05"}]}},
            {"id": "cash-void", "reward_usdc": {"amount": "0.05", "state": "void"}},
        ], has_more=True, since="unchanged-cursor", usdc_next=[{"id": "later"}])
        before = copy.deepcopy(response)
        code, command, got = run_cli(["--anonymous", "work", "list", "--kind", "rewarded", "--usdc-only"], response)
        self.assertEqual(code, 0)
        self.assertEqual([w["id"] for w in got["data"]["works"]], ["cash-promised", "conditional"])
        self.assertEqual(got["data"]["since"], "unchanged-cursor")
        self.assertTrue(got["data"]["has_more"])
        self.assertEqual(got["data"]["usdc_next"], [{"id": "later"}])
        self.assertEqual(got["data"]["works"][0]["reward_usdc"]["state"], "promised")
        # The filter is local: the board gets the same works.list as without the flag.
        self.assertEqual(command.call_args.args, ("works.list",))
        self.assertEqual(command.call_args.kwargs, {"kind": "rewarded"})
        self.assertEqual(response, before)

    def test_empty_filtered_page_keeps_cursor_and_rejects_zero_or_invalid_amounts(self):
        amounts = ["0", "0.000000", "NaN", "-1", "1e3", "0.0000001", " 1", True, 1, None]
        response = page([{"reward_usdc": {"amount": v}} for v in amounts]
                        + [{"standing": {"tiers": [{"usdc": "0.00"}, {"usdc": 5}]}}, {"standing": "x"}, "not-a-row"],
                        cursor="next-page", has_more=True)
        got = memo.filter_usdc_work_page(response)
        self.assertEqual(got["data"]["works"], [])
        self.assertEqual(got["data"]["cursor"], "next-page")
        self.assertTrue(got["data"]["has_more"])

    def test_smallest_usdc_unit_and_paid_receipt_remain_visible(self):
        works = [{"reward_usdc": {"amount": "0.000001", "state": "payable"}},
                 {"reward_usdc": {"amount": "0.05", "state": "paid"}}]
        self.assertEqual(memo.filter_usdc_work_page(page(works))["data"]["works"], works)

    def test_pending_standing_result_up_to_counts_as_usdc(self):
        # C1154: before its verdict a standing result's USDC reads up_to, at the top tier's amount.
        standing = "s" * 32
        pending = {"id": "result", "standing_id": standing, "reward_usdc": {"amount": "0.40", "up_to": True, "state": "promised"}}
        voided = {"id": "over-cap", "standing_id": standing, "reward_usdc": {"amount": "0.40", "state": "void"}}
        _, _, got = run_cli(["--anonymous", "work", "list", "--standing", standing, "--usdc-only"], page([pending, voided], standing={"open": 1}))
        self.assertEqual(got["data"]["works"], [pending])
        self.assertEqual(got["data"]["standing"], {"open": 1})

    def test_errors_and_default_cli_are_unchanged(self):
        error = {"ok": False, "error": "rate_limited"}
        self.assertIs(memo.filter_usdc_work_page(error), error)
        no_list = {"ok": True, "data": {"work": {}}}
        self.assertIs(memo.filter_usdc_work_page(no_list), no_list)
        response = page([{"id": "credits", "reward": {"amount": 50}}])
        _, _, got = run_cli(["--anonymous", "work", "list"], response)
        self.assertEqual(got, response)


if __name__ == "__main__":
    unittest.main()
