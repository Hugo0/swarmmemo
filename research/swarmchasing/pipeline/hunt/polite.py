"""Polite read-only fetcher: fixed User-Agent, robots.txt, >=1 s between requests per host.

Fetched bodies are untrusted data (often agent-to-agent text with prompt injections):
callers parse them in memory and keep counts only. Nothing here writes bodies to disk.
"""
import hashlib, json, os, secrets, time, urllib.error, urllib.parse, urllib.request, urllib.robotparser

UA = "swarmgraph-research/0.1 (+https://swarmmemo.com)"
_last, _robots = {}, {}
_SALT = secrets.token_bytes(16)  # per-run key, never stored: hashed ids cannot be linked across runs
LOG = {"requests": 0, "errors": 0, "robots_blocked": 0, "by_host": {}}


def hid(s):
    """Hash an identifier in memory; only the short digest is ever kept, and only within one run."""
    return hashlib.sha256(_SALT + s.encode("utf-8", "replace")).hexdigest()[:10]


def _robots_ok(url):
    p = urllib.parse.urlsplit(url)
    base = f"{p.scheme}://{p.netloc}"
    rp = _robots.get(base)
    if rp is None:
        rp = urllib.robotparser.RobotFileParser()
        try:
            _wait(p.netloc)
            req = urllib.request.Request(base + "/robots.txt", headers={"User-Agent": UA})
            with urllib.request.urlopen(req, timeout=20) as r:
                rp.parse(r.read().decode("utf-8", "replace").splitlines())
        except urllib.error.HTTPError as e:
            rp.parse(["User-agent: *", "Disallow: /"] if e.code in (401, 403) else [])
        except Exception:
            rp.parse([])
        _robots[base] = rp
    return rp.can_fetch(UA, url)


def _wait(host, gap=1.05):
    t = _last.get(host, 0) + gap - time.time()
    if t > 0:
        time.sleep(t)
    _last[host] = time.time()


def get(url, gap=1.05, robots=True, timeout=40, retries=2):
    """Return (status, text) or (None, reason)."""
    host = urllib.parse.urlsplit(url).netloc
    if robots and not _robots_ok(url):
        LOG["robots_blocked"] += 1
        return None, "robots"
    for attempt in range(retries + 1):
        _wait(host, gap)
        LOG["requests"] += 1
        LOG["by_host"][host] = LOG["by_host"].get(host, 0) + 1
        try:
            req = urllib.request.Request(url, headers={"User-Agent": UA, "Accept-Encoding": "identity"})
            with urllib.request.urlopen(req, timeout=timeout) as r:
                raw = r.read()
                cs = r.headers.get_content_charset() or "utf-8"
                return r.status, raw.decode(cs, "replace")
        except urllib.error.HTTPError as e:
            if e.code in (429, 503) and attempt < retries:
                time.sleep(30 * (attempt + 1)); continue
            LOG["errors"] += 1
            return None, f"http {e.code}"
        except Exception as e:
            if attempt < retries:
                time.sleep(5); continue
            LOG["errors"] += 1
            return None, type(e).__name__
    return None, "gave up"


def get_json(url, **kw):
    st, t = get(url, **kw)
    if st is None:
        return None, t
    try:
        return json.loads(t), None
    except Exception:
        return None, "bad json"


OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "data", "hunt")


def save(name, obj):
    os.makedirs(OUT, exist_ok=True)
    with open(os.path.join(OUT, name), "w") as f:
        json.dump(obj, f, indent=1, sort_keys=True)
