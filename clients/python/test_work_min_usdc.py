"""A worker can filter small USDC offers without rounding or changing the wire API."""
import copy
import unittest
from unittest.mock import patch

import swarmmemo as memo
from test_work_usdc_filter import page, run_cli


class WorkMinimumUSDCTests(unittest.TestCase):
    def test_cli_minimum_implies_cash_filter_and_does_not_change_wire(self):
        rows = [
            {"id": "credits", "reward": {"amount": 1000}},
            {"id": "below", "reward_usdc": {"amount": "0.099999", "state": "promised"}},
            {"id": "exact", "reward_usdc": {"amount": "0.10", "state": "promised"}},
            {"id": "tiered", "standing": {"tiers": [{"usdc": "0.02"}, {"usdc": "0.50"}]}},
            {"id": "void", "reward_usdc": {"amount": "10", "state": "void"}},
        ]
        response = page(rows, cursor="continue", has_more=True, usdc_next=[{"id": "later"}])
        before = copy.deepcopy(response)
        code, command, got = run_cli(["--anonymous", "work", "list", "--kind", "rewarded", "--min-usdc", "0.10"], response)
        self.assertEqual(code, 0)
        self.assertEqual([row["id"] for row in got["data"]["works"]], ["exact", "tiered"])
        self.assertEqual(command.call_args.kwargs, {"kind": "rewarded"})
        self.assertEqual(got["data"]["cursor"], "continue")
        self.assertEqual(got["data"]["usdc_next"], [{"id": "later"}])
        self.assertEqual(response, before)

    def test_library_minimum_preserves_empty_page_cursor_and_pending_upper_bound(self):
        pending = {"id": "pending", "reward_usdc": {"amount": "0.5", "state": "promised", "up_to": True}}
        response = page([pending], cursor="next", has_more=True)
        client = memo.Client("https://example.org")
        with patch.object(client, "command", return_value=response):
            self.assertEqual(client.works(min_usdc="0.50")["data"]["works"], [pending])
            got = client.works(min_usdc="0.500001")
        self.assertEqual(got["data"]["works"], [])
        self.assertEqual(got["data"]["cursor"], "next")
        self.assertTrue(got["data"]["has_more"])

    def test_invalid_minimum_is_refused_before_any_request(self):
        client = memo.Client("https://example.org")
        with patch.object(client, "command") as command:
            for value in ("0", "0.000000", "-1", "NaN", "1e-1", "0.0000001", "01", 0.1, True):
                with self.subTest(value=value), self.assertRaises(memo.UsageError):
                    client.works(min_usdc=value)
            command.assert_not_called()


if __name__ == "__main__":
    unittest.main()
