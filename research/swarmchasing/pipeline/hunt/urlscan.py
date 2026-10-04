#!/usr/bin/env python3
"""Track 1a: urlscan.io public search, anonymous tier, weekly totals per known host.

The anonymous tier only searches the last 30 days ("search_date_limit_days": 30), so the
Feb-to-now series cannot come from here; Feb-onward comes from Wayback CDX (cdx.py).
Only the `total` field of each search is kept. Stops at the first 429 (quota).
urlquery.net was skipped: its search API needs an account key.
"""
import sys, urllib.parse
from datetime import date, timedelta
import polite
from cdx import HOSTS

def main():
    today = date(2026, 10, 3)
    weeks = []
    d = today - timedelta(days=28)
    d -= timedelta(days=d.weekday())
    while d <= today:
        weeks.append(d); d += timedelta(days=7)
    out, stopped = {}, None
    for host in HOSTS:
        out[host] = {}
        for w in weeks:
            q = f'page.domain:"{host}" AND date:[{w.isoformat()} TO {(w + timedelta(days=6)).isoformat()}]'
            data, err = polite.get_json("https://urlscan.io/api/v1/search/?" + urllib.parse.urlencode({"q": q, "size": 1}),
                                        gap=3.0, robots=False, retries=0)
            if data is None:
                stopped = err
                break
            out[host][w.isoformat()] = data.get("total", 0)
        print(host, out[host], file=sys.stderr, flush=True)
        if stopped:
            break
    polite.save("urlscan_weekly.json", {"window_days": 30, "weekly_totals": out, "stopped": stopped,
                                        "note": "anonymous tier; totals only; first week partial (window clipped)"})

if __name__ == "__main__":
    main()
