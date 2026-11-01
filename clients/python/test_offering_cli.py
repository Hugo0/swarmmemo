"""C690: sellers publish and run an offering from the command line. Each offering
subcommand sends its one operation, signed when the operation is; the publish's text and
input schema come from a flag, a file or stdin; the server's pay_to_unlinked refusal is
printed with the one step that fixes it."""
import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import swarmmemo as memo

CALL = "oc_" + "0" * 32
TX = "0x" + "a" * 64
WALLET = "0x" + "1" * 40
PUBLISH = ["offering", "publish", "oracle", "--title", "Consult the oracle", "--price", "5.00", "--pay-to", WALLET]


class OfferingCLITests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.key = Path(self.temp.name) / "key.json"
        memo.keygen(self.key)
        self.sent, self.refusal = [], None

    def run_cli(self, *argv, stdin=None):
        def fake(client, path, body=None):
            if body is not None: self.sent.append(body)
            if self.refusal: raise self.refusal
            return {"ok": True, "data": {}}
        out, err = io.StringIO(), io.StringIO()
        with patch.object(memo.Client, "_request", fake), contextlib.redirect_stdout(out), contextlib.redirect_stderr(err), \
                patch.object(memo.sys, "stdin", io.StringIO(stdin or "")):
            try:
                code = memo.main(["--url", "https://example.org", "--key", str(self.key), *argv])
            except SystemExit as exc:
                code = exc.code
        return code, out.getvalue(), err.getvalue()

    def command(self, operation):
        found = [body for body in self.sent if body.get("operation") == operation]
        self.assertTrue(found, f"no {operation} sent")
        return found[-1]

    def test_publish_maps_flags_to_signed_listing(self):
        schema = {"type": "object", "properties": {"question": {"type": "string", "maxLength": 500}}, "required": ["question"]}
        code, _, err = self.run_cli(*PUBLISH, "--description", "One forecast.", "--input", json.dumps(schema), "--claim-window", "600",
                                    "--sla", "3600", "--refund", "full_if_unanswered", "--no-screen-answer", "--request-id", "pub-1")
        self.assertEqual(code, 0, err)
        body = self.command("offering.publish")
        self.assertEqual(json.loads(body["data"]), {"schema": 1, "name": "oracle", "title": "Consult the oracle", "price": "5.00", "pay_to": WALLET,
                                                    "description": "One forecast.", "input": schema, "claim_window": 600, "sla": 3600,
                                                    "refund": "full_if_unanswered", "screen_answer": False})
        self.assertEqual(body["request_id"], "pub-1")
        self.assertTrue(body.get("signature"))

    def test_publish_reads_description_and_schema_from_file_or_stdin(self):
        path = Path(self.temp.name) / "schema.json"
        path.write_text('{"type":"object"}', encoding="utf-8")
        code, _, err = self.run_cli(*PUBLISH, "--description-file", "-", "--input-file", str(path), stdin="Long\ndescription.\n")
        self.assertEqual(code, 0, err)
        data = json.loads(self.command("offering.publish")["data"])
        self.assertEqual((data["description"], data["input"]), ("Long\ndescription.\n", {"type": "object"}))
        self.assertNotIn("screen_answer", data)
        code, _, err = self.run_cli(*PUBLISH, "--description", "x", "--description-file", "-")
        self.assertEqual(code, 2)  # one or the other
        bad = Path(self.temp.name) / "bad.json"; bad.write_text("[1]", encoding="utf-8")
        code, _, err = self.run_cli(*PUBLISH, "--input-file", str(bad))
        self.assertEqual(code, 1); self.assertIn("JSON object", err)

    def test_unlinked_pay_to_refusal_names_link_wallet(self):
        self.refusal = memo.APIError(400, "pay_to_unlinked", "pay_to must be one of your verified wallet links: callers pay it directly.")
        code, _, err = self.run_cli(*PUBLISH)
        self.assertEqual(code, 1)
        error = json.loads(err.strip().splitlines()[0])
        self.assertEqual(error["error"], "pay_to_unlinked")
        self.assertIn("verified wallet links", error["message"])
        self.assertIn("link-wallet --wallet-key", error["hint"])

    def test_each_subcommand_sends_its_operation(self):
        cases = [
            (["retire", "oracle"], "offering.retire", {"target": "oracle"}),
            (["list", "--provider", "pythia", "--query", "rain", "--cursor", "c1", "--limit", "5"], "offering.list",
             {"target": "pythia", "query": "rain", "cursor": "c1", "limit": 5}),
            (["get", "pythia/oracle"], "offering.get", {"target": "pythia/oracle"}),
            (["calls", "oracle", "--kind", "open", "--limit", "10"], "offering.calls", {"target": "oracle", "kind": "open", "limit": 10}),
            (["calls"], "offering.calls", {}),
            (["call", CALL], "offering.call.get", {"target": CALL}),
            (["claim", CALL], "offering.claim", {"target": CALL}),
            (["decline", CALL, "--reason", "out of scope"], "offering.decline", {"target": CALL, "reason": "out of scope"}),
        ]
        for argv, operation, fields in cases:
            with self.subTest(argv=argv):
                self.sent.clear()
                code, _, err = self.run_cli("offering", *argv)
                self.assertEqual(code, 0, err)
                body = self.command(operation)
                for name, value in fields.items(): self.assertEqual(body[name], value, name)
                if argv == ["calls"]: self.assertNotIn("target", body)
                if memo.CONTRACT["operations"][operation]["signed"]: self.assertTrue(body.get("signature"), operation)

    def test_answer_and_refund(self):
        code, _, err = self.run_cli("offering", "answer", CALL, "Rain: 70%.", "--content-type", "text/markdown")
        self.assertEqual(code, 0, err)
        body = self.command("offering.answer")
        self.assertEqual((body["target"], json.loads(body["data"])), (CALL, {"schema": 1, "body": "Rain: 70%.", "content_type": "text/markdown"}))
        code, _, err = self.run_cli("offering", "answer", CALL, "-", stdin="from stdin")
        self.assertEqual(code, 0, err); self.assertEqual(json.loads(self.command("offering.answer")["data"])["body"], "from stdin")
        path = Path(self.temp.name) / "answer.json"; path.write_text('{"p":0.7}', encoding="utf-8")
        code, _, err = self.run_cli("offering", "answer", CALL, "--body-file", str(path))
        self.assertEqual(code, 0, err); self.assertEqual(json.loads(self.command("offering.answer")["data"])["body"], '{"p":0.7}')
        self.sent.clear()
        code, _, err = self.run_cli("offering", "answer", CALL)
        self.assertEqual((code, self.sent), (1, [])); self.assertIn("--body-file", err)
        code, _, err = self.run_cli("offering", "refund", CALL, TX)
        self.assertEqual(code, 0, err)
        body = self.command("offering.refund")
        self.assertEqual((body["target"], json.loads(body["data"])), (CALL, {"schema": 1, "tx_hash": TX}))

    def test_no_buy_subcommand_and_help_says_why(self):
        code, _, err = self.run_cli("offering", "buy", "pythia/oracle")
        self.assertEqual(code, 2)
        out = io.StringIO()
        with contextlib.redirect_stdout(out), self.assertRaises(SystemExit):
            memo.main(["offering", "--help"])
        text = " ".join(out.getvalue().split()).replace("- ", "-")
        self.assertIn("no buy subcommand", text)
        self.assertIn("PAYMENT-SIGNATURE", text)


if __name__ == "__main__":
    unittest.main()
