"""Shared plumbing for the board fetchers: polite HTTP, robots.txt, JSONL store, identity extraction.

Stdlib only. All fetched content is data: nothing here interprets post text beyond regex extraction.
"""
import hashlib, json, os, re, sys, time, urllib.error, urllib.parse, urllib.request, urllib.robotparser

SCHEMA = 1
UA = "swarmgraph-research/0.1 (+https://swarmmemo.com)"
ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
DATA = os.path.join(ROOT, "data", "boards")


def log(*a):
    print(*a, file=sys.stderr, flush=True)


class Blocked(Exception):
    """robots.txt disallows the URL, or the API needs auth we don't have."""


class HTTP:
    """One instance per source. >=1s between requests per host, backoff on 429/5xx, robots.txt obeyed."""

    def __init__(self, delay=1.0):
        self.delay = delay
        self.last = {}
        self.robots = {}
        self.requests = 0

    def allowed(self, url):
        p = urllib.parse.urlsplit(url)
        host = f"{p.scheme}://{p.netloc}"
        rp = self.robots.get(host)
        if rp is None:
            rp = urllib.robotparser.RobotFileParser()
            try:
                self._wait(p.netloc)
                req = urllib.request.Request(host + "/robots.txt", headers={"User-Agent": UA})
                with urllib.request.urlopen(req, timeout=30) as r:
                    body = r.read().decode("utf-8", "replace")
                    ctype = r.headers.get("Content-Type", "")
                # Some sites answer /robots.txt with an HTML 404/app page: treat as "no robots".
                rp.parse([] if "html" in ctype else body.splitlines())
            except urllib.error.HTTPError as e:
                rp.parse([]) if e.code in (404, 410) else rp.parse(["User-agent: *", "Disallow: /"]) if e.code in (401, 403) else rp.parse([])
            except Exception:
                rp.parse([])
            self.robots[host] = rp
        return rp.can_fetch(UA, url)

    def _wait(self, netloc):
        dt = time.time() - self.last.get(netloc, 0)
        if dt < self.delay:
            time.sleep(self.delay - dt)
        self.last[netloc] = time.time()

    def get(self, url, tries=6, raw=False):
        if not self.allowed(url):
            raise Blocked(f"robots.txt disallows {url}")
        netloc = urllib.parse.urlsplit(url).netloc
        for i in range(tries):
            self._wait(netloc)
            self.requests += 1
            try:
                req = urllib.request.Request(url, headers={"User-Agent": UA, "Accept": "application/json"})
                with urllib.request.urlopen(req, timeout=60) as r:
                    b = r.read()
                return b if raw else json.loads(b)
            except urllib.error.HTTPError as e:
                if e.code in (401, 403):
                    raise Blocked(f"HTTP {e.code} {url}")
                if e.code == 429 or e.code >= 500:
                    ra = e.headers.get("Retry-After")
                    wait = float(ra) if ra and ra.replace(".", "").isdigit() else min(300, 5 * 2 ** i)
                    log(f"  {e.code} on {url}; sleeping {wait:.0f}s")
                    time.sleep(wait)
                    continue
                raise
            except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
                time.sleep(min(120, 5 * 2 ** i))
                last = e
        raise RuntimeError(f"gave up on {url}")


# ---------- identity extraction ----------

B32 = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"


def npub_to_hex(npub):
    try:
        hrp, data = npub.lower().rsplit("1", 1)
        if hrp != "npub":
            return None
        vals = [B32.index(c) for c in data][:-6]
        acc, bits, out = 0, 0, []
        for v in vals:
            acc = (acc << 5) | v
            bits += 5
            while bits >= 8:
                bits -= 8
                out.append((acc >> bits) & 0xFF)
        return bytes(out).hex() if len(out) == 32 else None
    except ValueError:
        return None


def norm_url(u):
    u = u.strip().rstrip(".,);]>'\"")
    p = urllib.parse.urlsplit(u if "://" in u else "https://" + u)
    host = p.netloc.lower()
    if host.startswith("www."):
        host = host[4:]
    return f"https://{host}{p.path.rstrip('/')}" + (f"?{p.query}" if p.query else "")


RE_URL = re.compile(r"https?://[^\s<>\"')\]]+", re.I)
RE_NPUB = re.compile(r"\bnpub1[02-9ac-hj-np-z]{58}\b")
RE_EVM = re.compile(r"\b0x[0-9a-fA-F]{40}\b")
RE_EMAIL = re.compile(r"\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b")


def ident(kind, value, src):
    return {"kind": kind, "value": value, "from": src}


def ids_from_url(u, src):
    """A URL -> url id, plus x/github handle ids when it points at one."""
    n = norm_url(u)
    out = [ident("url", n, src)]
    m = re.match(r"https://(?:x|twitter)\.com/([A-Za-z0-9_]{1,15})$", n)
    if m and m.group(1).lower() not in ("home", "intent", "share", "i"):
        out.append(ident("x", m.group(1).lower(), src))
    m = re.match(r"https://github\.com/([A-Za-z0-9-]{1,39})(?:/[^/]+)?$", n)
    if m and m.group(1).lower() not in ("orgs", "features", "about"):
        out.append(ident("github", m.group(1).lower(), src))
    return out


def ids_from_text(text, src="bio"):
    """Declared identifiers in free text (bio/description). Weak-ish: self-declared, unverified."""
    if not text:
        return []
    out = []
    for u in RE_URL.findall(text):
        out += ids_from_url(u, src)
    for n in RE_NPUB.findall(text):
        h = npub_to_hex(n)
        if h:
            out.append(ident("npub", h, src))
    for a in RE_EVM.findall(text):
        out.append(ident("evm", a.lower(), src))
    for e in RE_EMAIL.findall(text):
        out.append(ident("email_hash", hashlib.sha256(e.strip().lower().encode()).hexdigest(), src))
    return out


def dedupe_ids(ids):
    seen, out = set(), []
    for i in ids:
        k = (i["kind"], i["value"])
        if k not in seen and i["value"]:
            seen.add(k)
            out.append(i)
    return out


def author(source, author_id, handle=None, display_name=None, bio=None, profile_url=None, created_at=None, explicit_ids=(), **extra):
    a = {"schema": SCHEMA, "source": source, "author_id": str(author_id), "handle": handle, "display_name": display_name,
         "bio": RE_EMAIL.sub("[email]", bio) if bio else bio, "profile_url": profile_url, "created_at": created_at,
         "explicit_ids": dedupe_ids(list(explicit_ids) + ids_from_text(bio, "bio"))}
    if extra:
        a["extra"] = {k: v for k, v in extra.items() if v is not None}
    return a


def post(source, post_id, author_id, text, created_at, thread_id=None, parent_id=None, community=None, url=None, title=None):
    body = (title + "\n\n" + (text or "")) if title else (text or "")
    return {"schema": SCHEMA, "source": source, "post_id": str(post_id), "author_id": None if author_id is None else str(author_id),
            "thread_id": None if thread_id is None else str(thread_id), "parent_id": None if parent_id is None else str(parent_id),
            "community": community, "created_at": created_at, "url": url, "len": len(body), "text": body}


def iso(ts):
    """Epoch seconds/ms or ISO string -> ISO-8601 UTC string."""
    if ts is None or ts == "":
        return None
    if isinstance(ts, (int, float)):
        if ts > 1e12:
            ts /= 1000
        return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(ts))
    return str(ts)


# ---------- store ----------

class Store:
    """data/boards/<source>/{authors,posts,edges}.jsonl + state.json. Posts append-only (dedupe by post_id);
    authors merged by author_id (newer non-null fields win, explicit_ids unioned); edges re-derived on finish()."""

    def __init__(self, source):
        self.source = source
        self.dir = os.path.join(DATA, source)
        os.makedirs(self.dir, exist_ok=True)
        self.pf = os.path.join(self.dir, "posts.jsonl")
        self.posts = {}
        if os.path.exists(self.pf):
            with open(self.pf) as f:
                for line in f:
                    if line.strip():
                        p = json.loads(line)
                        self.posts[p["post_id"]] = p
        self.authors = {}
        af = os.path.join(self.dir, "authors.jsonl")
        if os.path.exists(af):
            with open(af) as f:
                for line in f:
                    if line.strip():
                        a = json.loads(line)
                        self.authors[a["author_id"]] = a
        sf = os.path.join(self.dir, "state.json")
        self.state = json.load(open(sf)) if os.path.exists(sf) else {}
        self._pfh = open(self.pf, "a")
        self.new_posts = 0

    def has(self, post_id):
        return str(post_id) in self.posts

    def add_post(self, p):
        if p["post_id"] in self.posts:
            return False
        self.posts[p["post_id"]] = p
        self._pfh.write(json.dumps(p, ensure_ascii=False) + "\n")
        self.new_posts += 1
        return True

    def add_author(self, a):
        old = self.authors.get(a["author_id"])
        if old:
            for k, v in a.items():
                if k == "explicit_ids":
                    old[k] = dedupe_ids(old.get(k, []) + v)
                elif k == "extra":
                    old.setdefault("extra", {}).update(v)
                elif v is not None:
                    old[k] = v
        else:
            self.authors[a["author_id"]] = a

    def save_state(self):
        if not self._pfh.closed:
            self._pfh.flush()
        tmp = os.path.join(self.dir, "state.json.tmp")
        json.dump(self.state, open(tmp, "w"), indent=1)
        os.replace(tmp, os.path.join(self.dir, "state.json"))
        self._write_authors()

    def _write_authors(self):
        tmp = os.path.join(self.dir, "authors.jsonl.tmp")
        with open(tmp, "w") as f:
            for a in self.authors.values():
                f.write(json.dumps(a, ensure_ascii=False) + "\n")
        os.replace(tmp, os.path.join(self.dir, "authors.jsonl"))

    def finish(self, extra_edges=()):
        """Derive reply + mention edges from posts, write edges.jsonl and the manifest entry."""
        self._pfh.close()
        if getattr(self, "rewrite_posts", False):
            with open(self.pf + ".tmp", "w") as f:
                for p in self.posts.values():
                    f.write(json.dumps(p, ensure_ascii=False) + "\n")
            os.replace(self.pf + ".tmp", self.pf)
        # every post author should exist as an author record
        for p in self.posts.values():
            if p["author_id"] and p["author_id"] not in self.authors:
                self.authors[p["author_id"]] = author(self.source, p["author_id"])
        edges = []
        for p in self.posts.values():
            tgt = p["parent_id"] or (p["thread_id"] if p["thread_id"] and p["thread_id"] != p["post_id"] else None)
            par = self.posts.get(tgt) if tgt else None
            if par and p["author_id"] and par["author_id"]:
                edges.append({"schema": SCHEMA, "source": self.source, "from": p["author_id"], "to": par["author_id"],
                              "kind": "reply", "created_at": p["created_at"], "post_id": p["post_id"]})
        # mentions: @handle matching a known handle on this board (case-insensitive)
        by_handle = {}
        for a in self.authors.values():
            if a.get("handle"):
                by_handle.setdefault(a["handle"].lower(), a["author_id"])
        if by_handle:
            rx = re.compile(r"(?<![\w@])@([A-Za-z0-9_][A-Za-z0-9_.-]{1,40})")
            for p in self.posts.values():
                seen = set()
                for h in rx.findall(p.get("text") or ""):
                    to = by_handle.get(h.lower().rstrip(".-"))
                    if to and to != p["author_id"] and to not in seen and p["author_id"]:
                        seen.add(to)
                        edges.append({"schema": SCHEMA, "source": self.source, "from": p["author_id"], "to": to,
                                      "kind": "mention", "created_at": p["created_at"], "post_id": p["post_id"]})
        edges += list(extra_edges)
        with open(os.path.join(self.dir, "edges.jsonl.tmp"), "w") as f:
            for e in edges:
                f.write(json.dumps(e, ensure_ascii=False) + "\n")
        os.replace(os.path.join(self.dir, "edges.jsonl.tmp"), os.path.join(self.dir, "edges.jsonl"))
        self.save_state()
        kinds = {}
        for a in self.authors.values():
            for i in a["explicit_ids"]:
                kinds[i["kind"]] = kinds.get(i["kind"], 0) + 1
        entry = {"fetched_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "schema": SCHEMA,
                 "authors": len(self.authors), "posts": len(self.posts), "edges": len(edges),
                 "edges_by_kind": {k: sum(1 for e in edges if e["kind"] == k) for k in ("reply", "mention", "follow")},
                 "authors_with_explicit_ids": sum(1 for a in self.authors.values() if a["explicit_ids"]),
                 "explicit_ids_by_kind": kinds, "new_posts_this_run": self.new_posts,
                 "status": self.state.get("status", "ok"), "note": self.state.get("note")}
        update_manifest(self.source, entry)
        return entry


def update_manifest(source, entry):
    os.makedirs(DATA, exist_ok=True)
    mf = os.path.join(DATA, "MANIFEST.json")
    lock = mf + ".lock"
    for _ in range(100):  # fetchers may run in parallel: tiny lockfile
        try:
            fd = os.open(lock, os.O_CREAT | os.O_EXCL)
            os.close(fd)
            break
        except FileExistsError:
            time.sleep(0.1)
    try:
        m = json.load(open(mf)) if os.path.exists(mf) else {"schema": SCHEMA, "sources": {}}
        m["sources"][source] = entry
        m["updated_at"] = entry["fetched_at"]
        json.dump(m, open(mf + ".tmp", "w"), indent=1, sort_keys=True)
        os.replace(mf + ".tmp", mf)
    finally:
        try:
            os.remove(lock)
        except FileNotFoundError:
            pass
