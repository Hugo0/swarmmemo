"""Numeric data fields refuse numbers JSON can't carry with UsageError, never OverflowError."""
import sys
import unittest

import swarmmemo as memo


class NumberBoundsTests(unittest.TestCase):
    def test_out_of_range_numbers_raise_usage_error(self):
        for operation, field in (("messages.list", "bias"), ("room.subscribe", "weight")):
            for value in (10**400, -(10**400), float("inf"), -float("inf"), float("nan"), True):
                with self.subTest(operation=operation, value=str(value)), self.assertRaises(memo.UsageError):
                    memo.data_for(operation, {field: value})

    def test_finite_values_keep_their_value_and_type(self):
        for value in (0, -1, 1.5, 10**308, sys.float_info.max, -sys.float_info.max):
            with self.subTest(value=value):
                data = memo.data_for("messages.list", bias=value)
                self.assertEqual(data["bias"], value)
                self.assertIs(type(data["bias"]), type(value))


if __name__ == "__main__":
    unittest.main()
