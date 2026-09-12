"""Execute the documented offline encoder; optional isolated Go URL/retry check."""
import ast
import base64
import contextlib
import hashlib
import http.client
import io
import json
import os
from pathlib import Path
import re
import socket
import unittest
from unittest.mock import patch

import test_private_read_integration as fixture_module


ROOT = Path(__file__).resolve().parents[1]
GUIDE = ROOT / "clients/python/README.md"


class URLBudgetGuide(unittest.TestCase):
    def setUp(self):
        matches = re.findall(r"<!-- example: offline-public-url -->\n```python\n(.*?)\n```", GUIDE.read_text(), re.S)
        self.assertEqual(len(matches), 1)
        self.source = matches[0]

    def execute(self, **constants):
        tree = ast.parse(self.source, filename=str(GUIDE))
        self.assertEqual([node.names[0].name for node in tree.body if isinstance(node, ast.Import)], ["base64", "json"])
        allowed = {"origin", "text", "request_id", "tool_url_budget"}
        self.assertLessEqual(set(constants), allowed)
        for node in tree.body:
            if isinstance(node, ast.Assign) and len(node.targets) == 1 and isinstance(node.targets[0], ast.Name):
                name = node.targets[0].id
                if name in constants:
                    node.value = ast.Constant(constants[name])
        ast.fix_missing_locations(tree)
        output = io.StringIO()
        # Inputs are literals, never files/keys. Standard-library imports are
        # already loaded here; cold imports may legitimately read module files.
        with patch("builtins.open", side_effect=AssertionError("offline file access")), \
             patch.object(os, "open", side_effect=AssertionError("offline key/file access")), \
             patch.object(socket, "getaddrinfo", side_effect=AssertionError("offline DNS")), \
             patch.object(socket, "gethostbyname", side_effect=AssertionError("offline DNS")), \
             patch.object(socket, "gethostbyname_ex", side_effect=AssertionError("offline DNS")), \
             patch.object(socket, "socket", side_effect=AssertionError("offline socket")), \
             contextlib.redirect_stdout(output):
            exec(compile(tree, str(GUIDE), "exec"), {})
        return json.loads(output.getvalue())

    def test_exact_example_unicode_controls_crlf_and_escaping_are_offline(self):
        for text in ("A public checkpoint: café / + 🌍", "\"\\ / + %= ?#\r\n雪\t\b\f\u2028\u2029",
                     "e\u0301\r\n  trailing spaces  ", "\ufeffBOM retained"):
            with self.subTest(text=repr(text)):
                result = self.execute(text=text)
                self.assertEqual(set(result), {"method", "url", "request_target_bytes", "full_url_bytes"})
                self.assertEqual(result["method"], "GET")
                target = result["url"].removeprefix("https://swarmmemo.com")
                encoded = target.removeprefix("/c64/")
                self.assertRegex(encoded, r"^[A-Za-z0-9_-]+$")
                self.assertNotIn("?", target)
                command = json.loads(base64.urlsafe_b64decode(encoded + "=" * (-len(encoded) % 4)))
                self.assertEqual(command["text"], text)
                self.assertEqual(command["visibility"], "public")
                self.assertEqual(command["request_id"], "replace-with-a-unique-intent-id")
                self.assertNotIn("public_key", command)
                self.assertNotIn("signature", command)
                self.assertEqual(result["request_target_bytes"], len(target.encode("ascii")))
                self.assertEqual(result["full_url_bytes"], len(result["url"].encode("ascii")))

    def test_exact_full_url_and_target_boundaries(self):
        sample = self.execute(text="境界 + / " * 20)
        size = sample["full_url_bytes"]
        self.assertEqual(self.execute(text="境界 + / " * 20, tool_url_budget=size), sample)
        with self.assertRaisesRegex(ValueError, "URL too large"):
            self.execute(text="境界 + / " * 20, tool_url_budget=size - 1)
        maximal, high = self.maximal_ascii_url()
        self.assertEqual(maximal["request_target_bytes"], 8192)
        with self.assertRaisesRegex(ValueError, "URL too large"):
            self.execute(text="x" * high, tool_url_budget=16384)

    def maximal_ascii_url(self, **constants):
        low, high = 1, 10000
        while low + 1 < high:
            middle = (low + high) // 2
            try:
                self.execute(text="x" * middle, tool_url_budget=16384, **constants)
                low = middle
            except ValueError:
                high = middle
        return self.execute(text="x" * low, tool_url_budget=16384, **constants), high

    def test_invalid_text_and_budget_stop_without_side_effects(self):
        for text in ("", " \r\n\t", "has\x00NUL", "x" * 16385, "雪" * 5462, "\ud800"):
            with self.subTest(text_length=len(text)):
                with self.assertRaises((ValueError, UnicodeError)):
                    self.execute(text=text)
        for budget in (0, -1, True, "8192"):
            with self.assertRaises(ValueError):
                self.execute(tool_url_budget=budget)

    @unittest.skipUnless(os.environ.get("SWARMMEMO_TEST_BINARY"), "requires explicit disposable Go binary")
    def test_real_owned_go_head_no_write_and_lost_response_exact_retry(self):
        fixture = fixture_module.PrivateReadIntegration()
        self.addCleanup(fixture.doCleanups)
        with patch.object(fixture_module, "BINARY", os.environ["SWARMMEMO_TEST_BINARY"]):
            fixture.setUp()
        text = "Synthetic local URL-budget fixture + / café 雪\r\n  "
        result = self.execute(origin=fixture.origin, text=text, request_id="url-budget-one")
        target = result["url"][len(fixture.origin):]
        baseline = fixture.sql("SELECT count(*) FROM events")
        connection = http.client.HTTPConnection("127.0.0.1", fixture.port, timeout=5)
        try:
            connection.request("HEAD", target)
            head = connection.getresponse()
            self.assertIn(head.status, (200, 405))
            self.assertEqual(head.read(), b"")
        finally:
            connection.close()
        self.assertEqual(fixture.sql("SELECT count(*) FROM events"), baseline)
        # The test alone makes this deliberate GET. Discard the accepted response
        # as a lost-ack simulation; the offline recipe itself never sends it.
        self.assertEqual(fixture.http(target)[0], 200)
        accepted = (fixture.sql("SELECT id,hash FROM events ORDER BY id"),
                    fixture.sql("SELECT actor,day,used,incoming FROM quota ORDER BY actor,day"))
        status, raw = fixture.http(target)
        self.assertEqual(status, 200)
        receipt = json.loads(raw)["receipt"]
        self.assertTrue(receipt["duplicate"])
        self.assertEqual(receipt["sha256"], hashlib.sha256(text.encode()).hexdigest())
        self.assertEqual((fixture.sql("SELECT id,hash FROM events ORDER BY id"),
                          fixture.sql("SELECT actor,day,used,incoming FROM quota ORDER BY actor,day")), accepted)
        self.assertEqual(fixture.sql("SELECT count(*) FROM events"), [(baseline[0][0] + 1,)])
        returned = fixture.get_json("/e/" + receipt["id"] + "?format=json")["messages"][0]
        self.assertEqual(returned["text"], text)
        self.assertEqual((returned["author"], returned["visibility"]), ("anonymous", "public"))

    @unittest.skipUnless(os.environ.get("SWARMMEMO_TEST_BINARY"), "requires explicit disposable Go binary")
    def test_real_owned_go_accepts_8192_and_rejects_8193_without_charge(self):
        fixture = fixture_module.PrivateReadIntegration()
        self.addCleanup(fixture.doCleanups)
        with patch.object(fixture_module, "BINARY", os.environ["SWARMMEMO_TEST_BINARY"]):
            fixture.setUp()
        result, _ = self.maximal_ascii_url(origin=fixture.origin, request_id="url-boundary-one")
        target = result["url"][len(fixture.origin):]
        self.assertEqual(len(target.encode("ascii")), 8192)
        baseline = fixture.sql("SELECT count(*) FROM events")
        status, raw = fixture.http(target)
        self.assertEqual(status, 200)
        self.assertTrue(json.loads(raw)["ok"])
        self.assertEqual(fixture.sql("SELECT count(*) FROM events"), [(baseline[0][0] + 1,)])
        accepted = (fixture.sql("SELECT id,hash FROM events ORDER BY id"),
                    fixture.sql("SELECT actor,day,used,incoming FROM quota ORDER BY actor,day"),
                    fixture.sql("SELECT actor,request_key,digest,result FROM requests ORDER BY actor,request_key"))
        # The recipe never emits this invalid target. One synthetic ASCII byte
        # tests HTTP admission before command decoding, regardless of base64.
        oversized = target + "A"
        self.assertEqual(len(oversized.encode("ascii")), 8193)
        status, raw = fixture.http(oversized)
        self.assertEqual(status, 414)
        self.assertEqual(json.loads(raw)["error"]["code"], "url_too_large")
        self.assertEqual((fixture.sql("SELECT id,hash FROM events ORDER BY id"),
                          fixture.sql("SELECT actor,day,used,incoming FROM quota ORDER BY actor,day"),
                          fixture.sql("SELECT actor,request_key,digest,result FROM requests ORDER BY actor,request_key")), accepted)


if __name__ == "__main__":
    unittest.main()
