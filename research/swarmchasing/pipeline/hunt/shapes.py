#!/usr/bin/env python3
"""Track 3 deep-dive: capture shapes for the leads found by cdx.py. Index rows only; payloads
decoded in memory; only counts, length buckets, query-key names and referenced hosts are kept."""
import re, sys, urllib.parse
from collections import Counter, defaultdict
import polite
from cdx import b64shape, embedded, reg

def cdx(url, frm, to=None, match="prefix"):
    q = {"url": url, "matchType": match, "from": frm, "fl": "timestamp,original,statuscode,digest", "limit": "50000", "output": "json"}
    if to: q["to"] = to
    data, err = polite.get_json("https://web.archive.org/cdx/search/cdx?" + urllib.parse.urlencode(q), gap=2.0, robots=False, timeout=180)
    return (data or [])[1:], err

def lenb(n):
    return "<64" if n < 64 else "<256" if n < 256 else "<1k" if n < 1024 else "<4k" if n < 4096 else ">=4k"

def b64_profile(host, frm):
    rows, err = cdx(host + "/base64/", frm)
    day = defaultdict(Counter); hosts = defaultdict(Counter); lens = defaultdict(Counter); digests = defaultdict(set)
    for ts, orig, st, dg in rows:
        sh = b64shape(orig)
        if not sh: continue
        d = ts[:8]
        day[d][sh["kind"]] += 1; lens[d][lenb(sh["enc_len"])] += 1; digests[d].add(dg)
        for h in sh.get("hosts", []): hosts[d][h] += 1
    return {"error": err, "rows": len(rows), "daily_kinds": {d: dict(c) for d, c in sorted(day.items())},
            "daily_len_buckets": {d: dict(c) for d, c in sorted(lens.items())},
            "daily_distinct_payloads": {d: len(s) for d, s in sorted(digests.items())},
            "daily_ref_hosts": {d: dict(c.most_common(8)) for d, c in sorted(hosts.items())}}

def wrap_profile(host, frm):
    rows, err = cdx(host, frm, match="domain")
    tgt = defaultdict(Counter); ep = defaultdict(Counter); keys = defaultdict(Counter); lens = defaultdict(Counter); st_c = Counter()
    for ts, orig, st, dg in rows:
        e = embedded(orig, host)
        u = urllib.parse.unquote(orig)
        d = ts[:8]; st_c[st] += 1
        if e: tgt[d][e] += 1
        m = re.search(r"https?://[^/]*" + re.escape(e or "#none#") + r"(/[A-Za-z0-9_-]*)", u) if e else None
        if m: ep[d][(e or "") + m.group(1)] += 1
        inner = u[u.find(e):] if e and e in u else ""
        if "?" in inner:
            for k in urllib.parse.parse_qs(inner.split("?", 1)[1], keep_blank_values=True):
                if re.fullmatch(r"[A-Za-z_][A-Za-z0-9_-]{0,24}", k): keys[d][k] += 1
        lens[d][lenb(len(orig))] += 1
    return {"error": err, "rows": len(rows), "status": dict(st_c),
            "daily_targets": {d: dict(c.most_common(6)) for d, c in sorted(tgt.items())},
            "daily_inner_endpoints": {d: dict(c.most_common(5)) for d, c in sorted(ep.items())},
            "daily_inner_query_keys": {d: dict(c.most_common(8)) for d, c in sorted(keys.items())},
            "daily_len_buckets": {d: dict(c) for d, c in sorted(lens.items())}}

def main():
    out = {"base64": {}, "wrappers": {}}
    for h in ("httpbin.org", "pie.dev", "httpbun.com", "httpbingo.org"):
        out["base64"][h] = b64_profile(h, "20260201"); print(h, out["base64"][h]["rows"], file=sys.stderr)
    for h in ("jqp.vercel.app", "md.succ.ai", "markdown.new", "allorigins.hexlet.app"):
        out["wrappers"][h] = wrap_profile(h, "20260801"); print(h, out["wrappers"][h]["rows"], file=sys.stderr)
    out["wrappers"]["api.allorigins.win"] = wrap_profile("api.allorigins.win", "20260801")
    polite.save("cdx_shapes.json", out)

if __name__ == "__main__":
    main()
