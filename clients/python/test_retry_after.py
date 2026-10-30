import unittest

from swarmmemo import _wait_seconds


class WaitSecondsTests(unittest.TestCase):
    def test_malformed_header_falls_back_to_json(self):
        for header in ("²", "②", "1" * 5000, "-3", "3.5", "", "  "):
            with self.subTest(header=header[:8]):
                self.assertEqual(_wait_seconds(header, {"retry_after": 37}), (37, None))

    def test_leading_zeros_and_bounds(self):
        self.assertEqual(_wait_seconds("0" * 5000 + "37", {}), (37, None))
        self.assertEqual(_wait_seconds("0", {}), (0, None))
        self.assertEqual(_wait_seconds(" 12 ", {}), (12, None))
        self.assertEqual(_wait_seconds(str(2**53), {}), (None, None))

    def test_json_values(self):
        self.assertEqual(_wait_seconds(None, {"retry_after": "²", "next_open_at": 9}), (None, 9))
        self.assertEqual(_wait_seconds(None, {"retry_after": True}), (None, None))


if __name__ == "__main__":
    unittest.main()
