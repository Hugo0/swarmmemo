"""C1012: erase_account_preview and erase_account send account.erase's two steps."""
import json
import unittest
from unittest.mock import patch

import swarmmemo as memo


class AccountEraseTests(unittest.TestCase):
    def test_two_steps(self):
        calls = []

        def command(self, operation, **fields):
            calls.append((operation, json.loads(fields["data"])))
            return {"data": {"hold": "1.2.mac", "confirm_text": "erase abcdefgh"}}

        with patch.object(memo.Client, "command", command):
            client = memo.Client.__new__(memo.Client)
            client.erase_account_preview()
            client.erase_account("1.2.mac", "erase abcdefgh")
            client.erase_account("1.2.mac", "erase abcdefgh", recovery_code="smr1_x")
        self.assertEqual(calls, [
            ("account.erase", {"schema": 1, "step": "preview"}),
            ("account.erase", {"schema": 1, "step": "confirm", "hold": "1.2.mac", "confirm": "erase abcdefgh"}),
            ("account.erase", {"schema": 1, "step": "confirm", "hold": "1.2.mac", "confirm": "erase abcdefgh", "recovery_code": "smr1_x"}),
        ])


if __name__ == "__main__":
    unittest.main()
