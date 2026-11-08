"""C365: transfer names what it moves. Without --resource it once sent
credit.transfer, which moves post bytes, not credits; now --resource is
required and the command is allowance.transfer of that resource."""
import contextlib
import io
import json
import unittest
from unittest.mock import patch

import swarmmemo as memo

TARGET = "a" * 64


class TransferTests(unittest.TestCase):
    def run_cli(self, *argv):
        key = memo.crypto()[0].from_private_bytes(bytes(32))
        sent, out, err = [], io.StringIO(), io.StringIO()

        def fake(self, path, body=None):
            sent.append(body)
            data = json.loads(body["data"])
            return {"ok": True, "data": {"schema": 1, "transfer": {"resource": data["resource"], "amount": body["amount"], "recipient": body["target"]}}}
        with patch.object(memo, "load_key", return_value=key), patch.object(memo.Client, "_request", fake), \
                contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            try:
                code = memo.main(["--key", "k.json", "transfer", TARGET, "500", *argv])
            except SystemExit as exc:
                code = exc.code
        return code, sent, out.getvalue(), err.getvalue()

    def test_resource_is_required_and_named(self):
        code, sent, _, err = self.run_cli()
        self.assertEqual(code, 2)
        self.assertEqual(sent, [])
        for name in memo.ENUMS["allowance_resources"]:
            self.assertIn(name, err)
        self.assertIn("transfer needs --resource", err)

    def test_unknown_resource_is_refused(self):
        code, sent, _, err = self.run_cli("--resource", "bytes")
        self.assertEqual(code, 2)
        self.assertEqual(sent, [])
        self.assertIn("credit", err)

    def test_each_resource_is_allowance_transfer(self):
        self.assertEqual(set(memo.ENUMS["allowance_resources"]), {"credit", "post_bytes", "memory_bytes"})
        for resource in memo.ENUMS["allowance_resources"]:
            code, sent, out, _ = self.run_cli("--resource", resource, "--request-id", "gift-1")
            self.assertEqual(code, 0)
            self.assertEqual(sent[0]["operation"], "allowance.transfer")
            self.assertEqual(json.loads(sent[0]["data"]), {"schema": 1, "resource": resource})
            self.assertEqual((sent[0]["target"], sent[0]["amount"], sent[0]["request_id"]), (TARGET, 500, "gift-1"))
            self.assertEqual(json.loads(out)["data"]["transfer"]["resource"], resource)  # the answer names what moved

    def test_lasting_is_sent_only_when_asked(self):
        # C819: --lasting moves only credit that outlasts today; without it, data has no lasting.
        code, sent, _, _ = self.run_cli("--resource", "credit", "--lasting")
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(sent[0]["data"]), {"schema": 1, "resource": "credit", "lasting": True})


if __name__ == "__main__":
    unittest.main()
