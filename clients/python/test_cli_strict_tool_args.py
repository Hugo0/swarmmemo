"""CLI paid-tool safety: malformed/ambiguous JSON never reaches a network client.

All cases are in-memory. No wallet keys, network or server sessions involved.
Other paths already use strict_json; --args should obey the same contract.
"""
import types
import unittest
import contextlib
import io
from unittest.mock import patch

import swarmmemo as memo


class PaidToolArguments(unittest.TestCase):
    def test_well_formed_nested_args_preserve_data(self):
        sample = '{"budget":2,"payload":{"city":"Lisbon","extras":[1,true,null]}}'
        self.assertEqual(memo.json_object(sample), {
            "budget": 2, "payload": {"city":"Lisbon", "extras":[1, True, None]}
        })

    def test_duplicate_top_level_arguments_refused(self):
        with self.assertRaisesRegex(ValueError, "duplicate_json_field"):
            memo.json_object('{"recipient":"one","recipient":"two"}')

    def test_nested_duplicate_arguments_refused(self):
        with self.assertRaisesRegex(ValueError, "duplicate_json_field"):
            memo.json_object('{"order":{"amount":1,"amount":100}}')

    def test_nonfinite_numbers_refused_at_any_level(self):
        for text in (
            '{"max_cost":NaN}',
            '{"max_cost":Infinity}',
            '{"max_cost":-Infinity}',
            '{"payload":{"amount":NaN}}',
            '{"items":[1,Infinity]}',
        ):
            with self.subTest(text=text), self.assertRaisesRegex(
                ValueError, "invalid_json_number"
            ):
                memo.json_object(text)

    def test_non_object_still_refused(self):
        for text in ("[]", '"valid_json_but_not_object"', "42", "null", "true"):
            with self.subTest(text=text), self.assertRaisesRegex(
                ValueError, "args must be a JSON object"
            ):
                memo.json_object(text)

    def test_object_or_int_helper_keeps_integer_behavior_but_rejects_duplicate(self):
        self.assertEqual(memo.json_object_or_int("15"), 15)
        self.assertEqual(memo.json_object_or_int("-1"), -1)
        self.assertEqual(memo.json_object_or_int('{"limit":3}'), {"limit":3})
        with self.assertRaisesRegex(ValueError, "duplicate_json_field"):
            memo.json_object_or_int('{"limit":3,"limit":300}')

    def test_both_paid_cli_paths_do_not_dispatch_ambiguous_arguments(self):
        called = []

        class NoNetwork:
            def tools_call(self, *args):
                called.append(("tools", args))
                return {"ok": True}

            def call_url(self, *args):
                called.append(("call-url", args))
                return {"ok": True}

        for action, extra in [
            ("tools", {"tools_action":"call", "id":"tool:test"}),
            ("call-url", {"target_service":"public", "method":"check"}),
        ]:
            args = types.SimpleNamespace(
                action=action, args='{"recipients":["ok"],"recipients":["surprise"]}',
                max_cost=5, request_id="req1", **extra
            )
            with self.subTest(action=action), self.assertRaisesRegex(
                ValueError, "duplicate_json_field"
            ):
                memo.run_helper(args, NoNetwork())
        self.assertEqual(called, [], "No malformed paid tool request may dispatch")

    def test_legacy_paid_call_main_refuses_ambiguous_json_before_dispatch(self):
        # The legacy "call SERVICE METHOD JSON" is also a paid
        # service route; it must not silently coerce nonstandard JSON.
        sent = []

        class NoNetwork:
            def service_method(self, *args):
                sent.append(args)
                return {"ok": True}

        bad_samples = [
            ('{"recipient":"intended","recipient":"changed"}', "duplicate_json_field"),
            ('{"nested":{"amount":1,"amount":100}}', "duplicate_json_field"),
            ('{"max_cost":NaN}', "invalid_json_number"),
            ('{"max_cost":Infinity}', "invalid_json_number"),
            ('{"max_cost":-Infinity}', "invalid_json_number"),
        ]
        with patch.object(memo, "Client", return_value=NoNetwork()):
            for payload, code in bad_samples:
                with self.subTest(payload=payload):
                    out = io.StringIO()
                    err = io.StringIO()
                    with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                        self.assertEqual(memo.main([
                            "call","sample","service",payload,"--max-cost","5"
                        ]), 1)
                    self.assertEqual(sent, [], "Invalid paid command must NOT dispatch")
                    # The main CLI deliberately redacts the raw exception
                    # (which could disclose paid inputs / local secrets).
                    self.assertIn("SwarmMemo request failed (ValueError)", err.getvalue())
                    self.assertNotIn(payload, err.getvalue())

    def test_legacy_paid_call_preserves_valid_object_service_method(self):
        sent = []

        class NoNetwork:
            def service_method(self, *args):
                sent.append(args)
                return {"ok": True, "paid": False}

        out = io.StringIO()
        with patch.object(memo, "Client", return_value=NoNetwork()):
            with contextlib.redirect_stdout(out):
                self.assertEqual(memo.main([
                    "call","sample","service",'{"max_cost":2,"nested":{"city":"Lisbon"}}',
                    "--max-cost","5"
                ]), 0)
        self.assertEqual(len(sent),1)
        self.assertEqual(sent[0][0:2],("sample","service"))
        self.assertEqual(sent[0][2],{"max_cost":2,"nested":{"city":"Lisbon"}})
        self.assertEqual(sent[0][3],5)
        self.assertIn('"ok": true',out.getvalue())

    def test_both_paid_cli_paths_keep_valid_behavior(self):
        called = []

        class NoNetwork:
            def tools_call(self, *args):
                called.append(("tools",args))
                return {"ok":True}

            def call_url(self, *args):
                called.append(("call-url",args))
                return {"ok":True}

        c = NoNetwork()
        for action, extra in [
            ("tools", {"tools_action":"call","id":"tool:test"}),
            ("call-url", {"target_service":"public","method":"check"}),
        ]:
            args = types.SimpleNamespace(
                action=action, args='{"city":"Lisbon","max":2}',
                max_cost=5, request_id="req1", **extra
            )
            self.assertEqual(memo.run_helper(args,c), {"ok":True})
        self.assertEqual(len(called),2)
        self.assertEqual(called[0][1][1], {"city":"Lisbon","max":2})
        self.assertEqual(called[1][1][2], {"city":"Lisbon","max":2})


if __name__ == "__main__":
    unittest.main()
