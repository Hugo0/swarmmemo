#!/usr/bin/env python3
"""Track 2: small-wiki sweep for the collusion signature.

Targets: ProWiki/UseMod/Oddmuse-family wikis (the GET-writable engine family DSEWiki belongs to),
from the ProWiki farm itself, WikiIndex's engine categories, and wikis named in the collusion.wiki
revisions. Each wiki's public RecentChanges is read once (365 days where the engine allows),
<=1 request/s/host, robots.txt honoured (incl. Crawl-delay), bot-check pages skipped.

Editor names are hashed in memory with a per-run key, used only for counting, then discarded.
Page names are kept only for evidence pointers on wikis that score above baseline.
Page/summary text is untrusted: it is matched against regexes and never stored.
"""
import html, json, math, re, statistics, sys, urllib.parse
from collections import Counter, defaultdict
from datetime import date, datetime, timedelta
import polite

DE_MON = {m: i + 1 for i, m in enumerate("januar februar märz april mai juni juli august september oktober november dezember".split())}
DE_MON["maerz"] = 3; DE_MON["mrz"] = 3
EN_MON = {m: i + 1 for i, m in enumerate("january february march april may june july august september october november december".split())}
WELCOME = re.compile(r"(?i)^(willkommen|welcome|start(seite|page)?|home(page)?|hauptseite|main_?page|test(seite|page)?|sandbox|spielwiese|probier\w*|sandkasten|hallo|hello)")
BACKUP = re.compile(r"(?i)^zz|backup|kopie|copy\b|_bak|sicherung|mirror|archive?\d|v\d+$|\d{6}$")
AGENTISH = re.compile(r"(?i)agent|relay|helper|research|probe|cohort|bot\b|^ai[A-Z0-9_]|openai|gpt|claude|sector|window|round\d|answer|inbox|mailbox|sync")
MACHINE_NAME = re.compile(r"^(?=.*[A-Z].*[A-Z])(?=.*\d)[A-Za-z0-9_]{8,}$|(?i:agent|helper|researcher|probe|relay|bot)\w*\d")
RELAY_HOSTS = re.compile(r"(?i)jqp\.vercel\.app|md\.succ\.ai|markdown\.new|allorigins|r\.jina\.ai|workers\.dev|counterapi|httpbin\.org|pie\.dev|httpbun|cors\.lol|corsproxy|codetabs|da\.gd|is\.gd|vanderbi\.lt|ntfy\.sh|webhook\.site|pinggy|trycloudflare|ngrok|serveo|loca\.lt|postman-echo")
IPV = re.compile(r"^\d{1,3}(\.\d{1,3}){3}$|^[0-9a-f:]{6,}$", re.I)

SEED = [  # (id, rc-url template base, engine)
    ("wikiservice.at/dse", "https://www.wikiservice.at/dse/wiki.cgi", "prowiki"),
    ("wikiservice.at/probier", "https://www.wikiservice.at/probier/wiki.cgi", "prowiki"),
    ("wikiservice.at/fractal", "https://www.wikiservice.at/fractal/wiki.cgi", "prowiki"),
    ("wikiservice.at/gruender", "https://www.wikiservice.at/gruender/wiki.cgi", "prowiki"),
    ("wikiservice.at/buecher", "https://www.wikiservice.at/buecher/wiki.cgi", "prowiki"),
    ("wikiservice.at/support", "https://www.wikiservice.at/support/wiki.cgi", "prowiki"),
    ("wikiservice.at/wiki4d", "https://www.wikiservice.at/wiki4d/wiki.cgi", "prowiki"),
    ("prowiki.org/wiki4d", "https://www.prowiki.org/wiki4d/wiki.cgi", "prowiki"),
    ("dorfwiki.org", "https://www.dorfwiki.org/wiki.cgi", "prowiki"),
    ("wikiweb.at", "https://www.wikiweb.at/wiki.cgi", "prowiki"),
    ("wikiservice.at (root)", "https://www.wikiservice.at/wiki.cgi", "prowiki"),
    ("meatballwiki.org", "https://meatballwiki.org/recent", "html-generic"),
]


def wikiindex_targets():
    out = []
    for cat in ("ProWiki", "Oddmuse", "UseMod", "UseModWiki", "UseMod_Wiki"):
        q = {"action": "query", "generator": "categorymembers", "gcmtitle": "Category:" + cat, "gcmlimit": "max",
             "prop": "extlinks", "ellimit": "max", "format": "json"}
        cont = {}
        for _ in range(10):
            data, err = polite.get_json("https://wikiindex.org/api.php?" + urllib.parse.urlencode({**q, **cont}))
            if not data:
                break
            for p in (data.get("query") or {}).get("pages", {}).values():
                links = [l.get("*") or l.get("url") for l in p.get("extlinks", [])]
                links = [l for l in links if l and "wikiindex" not in l and not re.search(r"wikipedia|archive\.org|google|meatball|c2\.com", l)]
                if links:
                    out.append((cat, links[0]))
            if "continue" in data:
                cont = data["continue"]
            else:
                break
    return out


def rc_url(base, engine, days=365):
    if engine == "oddmuse":
        return base + ("&" if "?" in base else "?") + f"action=rc;raw=1;days={days};all=1;showedit=1"
    return base + ("&" if "?" in base else "?") + f"action=rc&days={days}"


def parse_date(txt):
    t = html.unescape(re.sub(r"<[^>]+>", "", txt)).strip().lower()
    m = re.match(r"(\d{1,2})\.\s*([a-zäöü]+)\s+(\d{4})", t)
    if m and m.group(2) in DE_MON:
        return date(int(m.group(3)), DE_MON[m.group(2)], int(m.group(1)))
    m = re.match(r"([a-z]+)\s+(\d{1,2}),?\s+(\d{4})", t)
    if m and m.group(1) in EN_MON:
        return date(int(m.group(3)), EN_MON[m.group(1)], int(m.group(2)))
    m = re.match(r"(\d{4})-(\d{2})-(\d{2})", t)
    if m:
        return date(*map(int, m.groups()))
    return None


def parse_usemod_html(text):
    """Yield (date, page, user, n_changes, summary) from UseMod/ProWiki-style RC html."""
    cur = None
    for m in re.finditer(r"<(?:p|h\d|b)>\s*<strong>(.*?)</strong>|<strong>([^<]{6,40}\d{4})</strong>|<li>(.*?)</li>", text, re.S | re.I):
        hdr = m.group(1) or m.group(2)
        if hdr:
            d = parse_date(hdr)
            if d:
                cur = d
            continue
        li = m.group(3)
        if cur is None or not li:
            continue
        hrefs = re.findall(r"href=['\"]([^'\"]+)['\"]", li)
        pages = [urllib.parse.unquote(h.split("?", 1)[1]) for h in hrefs if "?" in h and "action=" not in h]
        if not pages:
            continue
        page = pages[0]
        tail = li.rsplit(". . .", 1)[-1] if ". . ." in li else ""
        user = html.unescape(re.sub(r"<[^>]+>", "", tail)).strip(" .")
        um = re.search(r"href=['\"][^'\"]*\?([^'\"&]+)['\"]", tail)
        if um:
            user = urllib.parse.unquote(um.group(1))
        n = re.search(r"\((\d+)\s+(?:änderungen|&auml;nderungen|changes)", html.unescape(li), re.I)
        summ = re.search(r"<strong>\[(.*?)\]</strong>", li, re.S)
        yield cur, page, user, int(n.group(1)) if n else 1, (summ.group(1) if summ else "")


def parse_oddmuse_raw(text):
    for rec in text.split("\n\n"):
        f = dict(re.findall(r"^([a-z-]+): (.*)$", rec, re.M))
        if "title" in f and "last-modified" in f:
            d = parse_date(f["last-modified"][:10])
            if d:
                yield d, f["title"], f.get("generator", ""), 1, f.get("description", "")


def features(entries, start=None, end=None):
    """Per-wiki daily features + overall score. Users are hashed here and never leave this function raw."""
    ents = sorted(entries, key=lambda e: e[0])
    seen, daily = set(), defaultdict(lambda: {"edits": 0, "users": set(), "new": set(), "welcome": 0, "backup": 0,
                                              "agentish": 0, "machine_names": set(), "ip_edits": 0})
    per_user = Counter()
    warm = ents[0][0] + timedelta(days=14) if ents else None
    for d, page, user, n, summ in ents:
        u = polite.hid(user or "?")
        per_user[u] += n
    for d, page, user, n, summ in ents:
        u = polite.hid(user or "?")
        x = daily[d.isoformat()]
        x["edits"] += n; x["users"].add(u)
        if IPV.match(user or ""):
            x["ip_edits"] += n
        if u not in seen:
            seen.add(u)
            if d >= warm:  # first 14 days of the window only warm up the "seen" set
                x["new"].add(u)
        if WELCOME.search(page):
            x["welcome"] += n
        if BACKUP.search(page):
            x["backup"] += n
        if AGENTISH.search(page) or AGENTISH.search(summ or ""):
            x["agentish"] += n
        if user and not IPV.match(user) and MACHINE_NAME.search(user):
            x["machine_names"].add(u)
    days = {}
    for k, x in sorted(daily.items()):
        single = sum(1 for u in x["new"] if per_user[u] == 1)
        days[k] = {"edits": x["edits"], "users": len(x["users"]), "new_accounts": len(x["new"]), "single_edit_new": single,
                   "welcome_test_edits": x["welcome"], "backup_named_edits": x["backup"], "agentish_edits": x["agentish"],
                   "machine_named_users": len(x["machine_names"]), "ip_edits": x["ip_edits"]}
    return days


def score(days, exclude=None):
    """Burst score: worst day vs the wiki's own median day. exclude=(d0,d1) drops a known window."""
    ds = {k: v for k, v in days.items() if not (exclude and exclude[0] <= k <= exclude[1])}
    if not ds:
        return 0.0, None, {}
    best, bk = -1, None
    for k, v in ds.items():
        s = (math.log1p(v["new_accounts"]) + math.log1p(v["single_edit_new"]) + math.log1p(v["machine_named_users"]) * 1.5
             + math.log1p(v["welcome_test_edits"]) * 0.5 + math.log1p(v["backup_named_edits"]) * 0.5 + math.log1p(v["agentish_edits"]))
        if s > best:
            best, bk = s, k
    return round(best, 3), bk, ds[bk]


def relay_mentions(base, pages):
    """Fetch a few flagged pages; count relay-host mentions only."""
    c, fetched = Counter(), 0
    for p in pages[:12]:
        st, t = polite.get(base + "?" + urllib.parse.quote(p, safe=""))
        if st:
            fetched += 1
            for h in RELAY_HOSTS.findall(t):
                c[h.lower()] += 1
        del t
    return fetched, dict(c)


def sweep(targets):
    res = {}
    for wid, base, engine in targets:
        url = rc_url(base, engine)
        st, text = polite.get(url, timeout=45, retries=1)
        if st is None:
            res[wid] = {"base": base, "status": text}; print(wid, text, file=sys.stderr); continue
        if re.search(r"<title>\s*Bot Check", text[:2000], re.I):
            res[wid] = {"base": base, "status": "bot-check page (skipped)"}; continue
        if engine == "oddmuse" or text.lstrip().startswith("title:") or "\ntitle: " in text[:3000]:
            ents = list(parse_oddmuse_raw(text)); engine = "oddmuse"
        else:
            ents = list(parse_usemod_html(text))
        if not ents and engine == "auto":
            st, text = polite.get(rc_url(base, "oddmuse"), timeout=120)
            ents = list(parse_oddmuse_raw(text)) if st else []
            engine = "oddmuse" if ents else engine
        # keep page names only transiently, for evidence on flagged days
        page_days = defaultdict(Counter)
        for d, page, user, n, summ in ents:
            if WELCOME.search(page) or BACKUP.search(page) or AGENTISH.search(page) or AGENTISH.search(summ or ""):
                page_days[d.isoformat()][page] += n
        days = features(ents)
        del text
        res[wid] = {"base": base, "engine": engine, "status": "ok", "entries": len(ents),
                    "first": min((e[0] for e in ents), default=None), "last": max((e[0] for e in ents), default=None),
                    "days": days, "_page_days": page_days}
        res[wid]["first"] = res[wid]["first"] and res[wid]["first"].isoformat()
        res[wid]["last"] = res[wid]["last"] and res[wid]["last"].isoformat()
        print(wid, engine, len(ents), file=sys.stderr, flush=True)
    return res


def main():
    targets = list(SEED)
    known = {urllib.parse.urlsplit(b).netloc.replace("www.", "") + urllib.parse.urlsplit(b).path for _, b, _ in SEED}
    for cat, link in wikiindex_targets():
        p = urllib.parse.urlsplit(link)
        if p.scheme not in ("http", "https") or re.search(r"archive\.(is|ph|today)", p.netloc):
            continue
        path = p.path if re.search(r"\.(cgi|pl)$", p.path) else (p.path.rstrip("/") + "/" if p.path else "/")
        key = p.netloc.replace("www.", "") + path
        if key in known or any(key.startswith(k.rsplit("/", 1)[0]) and "wikiservice" in k for k in known if "wikiservice" in key):
            continue
        known.add(key)
        base = f"{p.scheme}://{p.netloc}{p.path}" if re.search(r"\.(cgi|pl)$", p.path) else f"{p.scheme}://{p.netloc}{p.path}"
        targets.append((p.netloc + p.path, base, "prowiki" if cat == "ProWiki" else "auto"))
    print(len(targets), "targets", file=sys.stderr)
    res = sweep(targets)
    # scoring: DSEWiki known window excluded for the re-emergence check; baseline from all other wikis
    KNOWN = ("2026-05-11", "2026-07-31")
    rows = []
    for wid, r in res.items():
        if r.get("status") != "ok" or not r["entries"]:
            continue
        s_all, d_all, f_all = score(r["days"])
        s_out, d_out, f_out = score(r["days"], KNOWN)
        r.update({"score_all": s_all, "peak_day_all": d_all, "peak_all": f_all,
                  "score_outside_known": s_out, "peak_day_outside_known": d_out, "peak_outside_known": f_out})
        rows.append(s_out)
    base = statistics.median(rows) if rows else 0
    mad = statistics.median([abs(x - base) for x in rows]) if rows else 0
    thr = base + 3 * max(mad, 0.5)
    for wid, r in res.items():
        if r.get("status") != "ok" or not r.get("entries"):
            r.pop("_page_days", None); continue
        r["above_baseline"] = r["score_outside_known"] > thr
        pd = r.pop("_page_days")
        if r["above_baseline"] or wid.startswith("wikiservice.at/dse"):
            ev = {}
            for dk in sorted({r["peak_day_outside_known"], r["peak_day_all"]} - {None}):
                pages = [p for p, _ in pd.get(dk, Counter()).most_common(8)]
                fetched, rel = relay_mentions(r["base"], pages) if pages else (0, {})
                ev[dk] = {"flagged_pages": [r["base"] + "?" + urllib.parse.quote(p, safe="") for p in pages],
                          "pages_fetched": fetched, "relay_host_mentions": rel}
            r["evidence"] = ev
    polite.save("wiki_sweep.json", {"baseline_median": base, "baseline_mad": mad, "threshold": thr,
                                    "known_window_excluded": KNOWN, "wikis": res, "requests": polite.LOG})


if __name__ == "__main__":
    main()
