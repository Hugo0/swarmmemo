"""The Python verification CLI must match the Go board flaky-count bounds.
These tests run locally without a network, a key or any funds.
"""
import unittest
import swarmmemo as memo

class FlakyCLIParityTests(unittest.TestCase):
    def test_valid_boundary(self):
        for payload in ("tests=flaky:1/2", "tests=flaky:999/1000",
                        "tests=flaky:7/11@step_timeout:three timeouts"):
            with self.subTest(payload=payload):
                d = memo.parse_check(payload)
                self.assertEqual(d["state"], "flaky")
                self.assertLess(d["passes"], d["runs"])

    def test_refuses_impossible_counts_before_any_signed_command(self):
        for payload in ("tests=flaky:0/0", "tests=flaky:1/1", "tests=flaky:0/2",
                        "tests=flaky:2/2", "tests=flaky:3/2",
                        "tests=flaky:10/3", "tests=flaky:1/1001"):
            with self.subTest(payload=payload), self.assertRaises(ValueError):
                memo.parse_check(payload)

if __name__ == "__main__":
    unittest.main()
