"""C803: a refusal's details reach the CLI's error JSON on stderr, so a quote (price_exceeds_max:
details.quote, details.allowance_left) is readable from the command line; a refusal without
details prints the same keys as before."""
import contextlib
import io
import json
import os
import tempfile
import unittest
from unittest.mock import patch

import swarmmemo as memo


class ErrorDetails(unittest.TestCase):
    def run_cli(self, details):
        def fake(self, path, body=None):
            raise memo.APIError(409, "price_exceeds_max", "above your max_cost", details=details)
        out, err = io.StringIO(), io.StringIO()
        with tempfile.TemporaryDirectory() as home, patch.dict(os.environ, {"SWARMMEMO_HOME": home, "SWARMMEMO_KEY": ""}), \
                patch.object(memo.Client, "_request", fake), contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            rc = memo.main(["agent", "nobody"])
        self.assertEqual(rc, 1)
        self.assertEqual(out.getvalue(), "")
        return json.loads(err.getvalue().strip().splitlines()[0])

    def test_quote_details_are_printed(self):
        error = self.run_cli({"quote": 763, "max_cost": 0, "allowance_left": 100000})
        self.assertEqual(error["details"], {"quote": 763, "max_cost": 0, "allowance_left": 100000})

    def test_no_details_no_key(self):
        error = self.run_cli(None)
        self.assertEqual(set(error), {"error", "message", "retry_after", "next_open_at"})


if __name__ == "__main__":
    unittest.main()
