# SPDX-License-Identifier: Apache-2.0
"""Tests for scripts/public_data_parity.py (standard library only)."""
from __future__ import annotations

import contextlib
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import public_data_parity as parity  # noqa: E402

# A reference answer as the ported adapter gives it (Python floats), and the
# same answer as a public_data item.
REFERENCE = {
    "station": "USW00014755", "requested_station": "kmwn", "start_date": "2026-01-10", "end_date": "2026-01-12",
    "count": 2, "peak_gust_max_mph": 134.2, "peak_gust_max_date": "2026-01-11",
    "days": [
        {"date": "2026-01-11", "peak_gust_mph": 134.2, "peak_gust_ms": 60.0, "peak_gust_element": "WSFG", "tmax_c": -2.2},
        {"date": "2026-01-10", "peak_gust_mph": 117.0, "peak_gust_ms": 52.3, "peak_gust_element": "WSF2", "tmax_c": -1.7},
    ],
}
ITEM = {
    "dataset": "noaa_station_daily", "schema_version": 1, "params": {"station": "kmwn"}, "stale": False,
    "data": {
        "station": "USW00014755", "requested_station": "kmwn", "start_date": "2026-01-10", "end_date": "2026-01-12",
        "count": 2, "peak_gust_max_mph": 134.2, "peak_gust_max_date": "2026-01-11", "truncated": False,
        "days": [
            {"date": "2026-01-10", "peak_gust_mph": 117, "peak_gust_ms": 52.3, "peak_gust_element": "WSF2", "tmax_c": -1.7, "precip_mm": None},
            {"date": "2026-01-11", "peak_gust_mph": 134.2, "peak_gust_ms": 60, "peak_gust_element": "WSFG", "tmax_c": -2.2, "precip_mm": None},
        ],
    },
}


class ParityTest(unittest.TestCase):
    def run_parity(self, dataset, ref, got, *extra):
        with tempfile.TemporaryDirectory() as d:
            a, b = os.path.join(d, "ref.json"), os.path.join(d, "got.json")
            Path(a).write_text(json.dumps(ref), encoding="utf-8")
            Path(b).write_text(json.dumps(got), encoding="utf-8")
            out, err = io.StringIO(), io.StringIO()
            with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                code = parity.main([dataset, a, b, "--json", *extra])
            return code, (json.loads(out.getvalue()) if out.getvalue() else None), err.getvalue()

    def test_agreeing_values_pass(self):
        code, report, _ = self.run_parity("noaa_station_daily", REFERENCE, ITEM)
        self.assertEqual(code, 0, report)
        self.assertEqual(report["differences"], [])

    def test_a_changed_number_fails(self):
        got = json.loads(json.dumps(ITEM))
        got["data"]["days"][1]["peak_gust_ms"] = 60.1
        code, report, _ = self.run_parity("noaa_station_daily", REFERENCE, got)
        self.assertEqual(code, 1)
        self.assertEqual(report["differences"][0]["path"], ".days#.peak_gust_ms")

    def test_tolerance(self):
        got = json.loads(json.dumps(ITEM))
        got["data"]["peak_gust_max_mph"] = 134.20001
        self.assertEqual(self.run_parity("noaa_station_daily", REFERENCE, got)[0], 1)
        self.assertEqual(self.run_parity("noaa_station_daily", REFERENCE, got, "--abs-tol", "1e-3")[0], 0)

    def test_missing_value_fails_but_missing_null_does_not(self):
        got = json.loads(json.dumps(ITEM))
        del got["data"]["peak_gust_max_date"]
        self.assertEqual(self.run_parity("noaa_station_daily", REFERENCE, got)[0], 1)
        ref = dict(REFERENCE, note=None, truncated=False)
        self.assertEqual(self.run_parity("noaa_station_daily", ref, ITEM)[0], 0)

    def test_extras_fail_only_when_strict(self):
        got = json.loads(json.dumps(ITEM))
        got["data"]["new_field"] = 1
        code, report, _ = self.run_parity("noaa_station_daily", REFERENCE, got)
        self.assertEqual((code, report["extras"]), (0, [".new_field"]))
        self.assertEqual(self.run_parity("noaa_station_daily", REFERENCE, got, "--strict-extra")[0], 1)

    def test_prose_is_ignored(self):
        ref = dict(REFERENCE, note="showing 31 of 45 days")
        got = json.loads(json.dumps(ITEM))
        got["data"]["note"] = "a differently worded note"
        self.assertEqual(self.run_parity("noaa_station_daily", ref, got)[0], 0)
        self.assertEqual(self.run_parity("noaa_station_daily", ref, got, "--no-default-ignore")[0], 1)

    def test_single_bank_reference_is_reshaped(self):
        ref = {"today": "2026-08-15", "what": "rate", "bank": "ECB — European Central Bank (euro area)", "policy_rate_pct": 2.25,
               "rate_as_of": "2026-08-12", "rate_note": "prose"}
        got = {"dataset": "cb_policy_rates", "data": {"today": "2026-08-15", "what": "rate", "banks": {"ECB": {
            "bank": "ECB — European Central Bank (euro area)", "policy_rate_pct": 2.25, "rate_as_of": "2026-08-12", "rate_may_be_stale": False}}}}
        self.assertEqual(self.run_parity("cb_policy_rates", ref, got)[0], 0)
        got["data"]["banks"]["ECB"]["policy_rate_pct"] = 2.0
        self.assertEqual(self.run_parity("cb_policy_rates", ref, got)[0], 1)

    def test_bulk_needs_an_index_and_stale_warns(self):
        bulk = {"result": {"schema_version": 1, "results": [dict(ITEM, stale=True, fetched_at="2026-01-13T00:00:00Z")]}}
        code, _, err = self.run_parity("noaa_station_daily", REFERENCE, bulk)
        self.assertEqual(code, 2)
        self.assertIn("--index", err)
        code, report, _ = self.run_parity("noaa_station_daily", REFERENCE, bulk, "--index", "0")
        self.assertEqual(code, 0)
        self.assertIn("stale", report["warnings"][0])

    def test_a_failed_item_is_bad_input(self):
        code, _, err = self.run_parity("noaa_station_daily", REFERENCE, {"dataset": "x", "data": None, "error": {"code": "upstream_busy"}})
        self.assertEqual(code, 2)
        self.assertIn("failed", err)


if __name__ == "__main__":
    unittest.main()
