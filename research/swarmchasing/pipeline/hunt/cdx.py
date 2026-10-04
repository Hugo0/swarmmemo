#!/usr/bin/env python3
"""Tracks 1b + 3: Wayback CDX capture counts for relay/reader/echo hosts, Feb 2026 to now.

Index rows only (timestamp, URL, status); no page is fetched. /base64/ payloads are decoded
in memory to classify their shape and the hosts they name; no decoded text is stored.
Embedded targets (reader-proxy URLs that wrap another URL) are reduced to registrable host.
Run: nice -n 19 python3 hunt/cdx.py
"""
import base64, binascii, re, sys, urllib.parse
from collections import Counter, defaultdict
from datetime import datetime, timezone
import polite

FROM = "20260201"
HOSTS = {  # host -> category
    "jqp.vercel.app": "relay", "md.succ.ai": "reader", "markdown.new": "reader", "r.jina.ai": "reader",
    "api.allorigins.win": "cors", "allorigins.hexlet.app": "cors", "cors.bwa.workers.dev": "cors",
    "api.counterapi.dev": "counter", "counterapi.dev": "counter",
    "httpbin.org": "echo", "pie.dev": "echo", "httpbun.com": "echo", "httpbingo.org": "echo",
    "postman-echo.com": "echo", "pure.md": "reader", "md.dhr.wtf": "reader", "corsproxy.io": "cors",
    "api.codetabs.com": "cors", "corsmirror.com": "cors", "jsonplaceholder.typicode.com": "echo",
}
CAP = 300000
URLRE = re.compile(r"https?://([a-z0-9.-]+\.[a-z]{2,})", re.I)


def week(ts):
    d = datetime.strptime(ts[:8], "%Y%m%d").replace(tzinfo=timezone.utc)
    return d.date().fromordinal(d.date().toordinal() - d.weekday()).isoformat()


def reg(host):
    h = host.lower().strip(".")
    parts = h.split(".")
    if len(parts) >= 3 and parts[-2] in ("co", "com", "org", "ac", "gov", "net") and len(parts[-1]) == 2:
        return ".".join(parts[-3:])
    if parts[-2:] in (["vercel", "app"], ["workers", "dev"], ["github", "io"], ["hf", "space"], ["netlify", "app"],
                      ["pages", "dev"], ["onrender", "com"], ["herokuapp", "com"], ["glitch", "me"], ["repl", "co"]):
        return ".".join(parts[-3:])
    return ".".join(parts[-2:])


def embedded(orig, host):
    """Host of a URL wrapped by a reader/cors proxy, or None."""
    u = urllib.parse.unquote(urllib.parse.unquote(orig))
    i = u.lower().find(host)
    rest = u[i + len(host):] if i >= 0 else u
    m = URLRE.search(rest)
    if m:
        return reg(m.group(1))
    m = re.match(r"/+([a-z0-9-]+\.)+[a-z]{2,}", rest, re.I)
    return reg(m.group(0).strip("/")) if m else None


def b64shape(orig):
    m = re.search(r"/base64/([^?#]*)", orig)
    if not m:
        return None
    s = urllib.parse.unquote(m.group(1))
    sh = {"enc_len": len(s)}
    try:
        raw = base64.urlsafe_b64decode(s.replace("+", "-").replace("/", "_") + "=" * (-len(s) % 4))
        t = raw.decode("utf-8")
    except (binascii.Error, ValueError, UnicodeDecodeError):
        sh["kind"] = "undecodable"
        return sh
    tl = t.lower()
    if "<script" in tl or "fetch(" in tl or "xmlhttprequest" in tl:
        k = "html+script"
    elif "<form" in tl:
        k = "html+form"
    elif "<html" in tl or "<!doctype" in tl or re.search(r"<(div|p|body|a|meta|h1)\b", tl):
        k = "html"
    elif t.strip()[:1] in "{[":
        k = "json-like"
    elif len(t) < 24:
        k = "short-text"
    else:
        k = "text"
    sh["kind"] = k
    sh["hosts"] = sorted({reg(h) for h in URLRE.findall(t)})
    del t, tl, raw
    return sh


def rows(host):
    out, key, trunc = [], None, False
    while True:
        q = {"url": host, "matchType": "domain", "from": FROM, "fl": "timestamp,original,statuscode",
             "limit": "25000", "showResumeKey": "true", "output": "json"}
        if key:
            q["resumeKey"] = key
        data, err = polite.get_json("https://web.archive.org/cdx/search/cdx?" + urllib.parse.urlencode(q),
                                    gap=2.0, robots=False, timeout=120)
        if data is None:
            return out, err, trunc
        key = None
        if len(data) >= 2 and data[-2] == []:
            key = data[-1][0]; data = data[:-2]
        out.extend(data[1:] if data and data[0] and data[0][0] == "timestamp" else data)
        if len(out) >= CAP:
            return out, None, True
        if not key:
            return out, None, trunc


def main():
    res = {}
    for host, cat in HOSTS.items():
        rs, err, trunc = rows(host)
        wk, day, emb_wk, b64 = Counter(), Counter(), defaultdict(Counter), defaultdict(Counter)
        b64_hosts, b64_len, paths = defaultdict(Counter), defaultdict(list), Counter()
        for r in rs:
            if len(r) < 2:
                continue
            ts, orig = r[0], r[1]
            w = week(ts); wk[w] += 1; day[ts[:8]] += 1
            p = urllib.parse.urlsplit(orig).path
            paths["/" + p.strip("/").split("/")[0] if p.strip("/") else "/"] += 1
            if cat in ("reader", "cors", "relay"):
                e = embedded(orig, host)
                if e and e != reg(host):
                    emb_wk[e][w] += 1
            if cat == "echo":
                sh = b64shape(orig)
                if sh:
                    b64[w][sh["kind"]] += 1
                    b64_len[w].append(sh["enc_len"])
                    for h in sh.get("hosts", []):
                        b64_hosts[w][h] += 1
        emb_tot = Counter({h: sum(c.values()) for h, c in emb_wk.items()})
        res[host] = {
            "category": cat, "captures": len(rs), "error": err, "truncated_at_cap": trunc,
            "weekly": dict(sorted(wk.items())), "daily": dict(sorted(day.items())),
            "top_paths": dict(paths.most_common(12)),
            "embedded_targets_total": dict(emb_tot.most_common(60)),
            "embedded_targets_weekly": {h: dict(sorted(emb_wk[h].items())) for h, _ in emb_tot.most_common(60)},
            "base64_weekly_kinds": {w: dict(c) for w, c in sorted(b64.items())},
            "base64_weekly_len_median": {w: sorted(v)[len(v) // 2] for w, v in sorted(b64_len.items())},
            "base64_weekly_hosts": {w: dict(c.most_common(15)) for w, c in sorted(b64_hosts.items())},
        }
        print(f"{host}: {len(rs)} captures err={err} trunc={trunc} weeks={len(wk)}", file=sys.stderr, flush=True)
    polite.save("cdx_series.json", {"from": FROM, "hosts": res, "requests": polite.LOG})


if __name__ == "__main__":
    main()
