#!/usr/bin/env python3
"""Page through every public SwarmMemo message (metadata only, no text) into data/swarmmemo.jsonl.

Stdlib only. Forward traversal from cursor=start, ~1 request/second.
Usage: python3 fetch/swarmmemo.py [--base https://swarmmemo.com] [--out data/swarmmemo.jsonl] [--delay 1.0]
"""
import argparse, base64, hashlib, json, os, sys, time, urllib.parse, urllib.request, urllib.error

KEEP = ("id", "sequence", "room", "page", "author", "reply_to", "kind", "visibility", "created_at", "via", "author_handle")


def fingerprint(msg):
    """One identity form: the 64-hex sha256 fingerprint of the raw Ed25519 public key, or 'anonymous'."""
    a = msg.get("author") or "anonymous"
    if a == "anonymous":
        return a
    if len(a) == 64 and all(c in "0123456789abcdef" for c in a):
        return a
    pk = msg.get("public_key") or a  # a raw base64url public key slipped through: hash it
    raw = base64.urlsafe_b64decode(pk + "=" * (-len(pk) % 4))
    return hashlib.sha256(raw).hexdigest()


def get(url, tries=5):
    for i in range(tries):
        try:
            req = urllib.request.Request(url, headers={"User-Agent": "swarmgraph/0.1 (hackathon visualizer)"})
            with urllib.request.urlopen(req, timeout=30) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            if e.code in (429, 502, 503, 504):
                time.sleep(float(e.headers.get("Retry-After") or 2 ** i))
                continue
            raise
        except urllib.error.URLError:
            time.sleep(2 ** i)
    raise SystemExit(f"giving up on {url}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="https://swarmmemo.com")
    ap.add_argument("--out", default=os.path.join(os.path.dirname(__file__), "..", "data", "swarmmemo.jsonl"))
    ap.add_argument("--delay", type=float, default=1.0)
    args = ap.parse_args()
    os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
    cursor, seen, n, pages = "start", set(), 0, 0
    tmp = args.out + ".tmp"
    with open(tmp, "w") as f:
        while True:
            q = urllib.parse.urlencode({"cursor": cursor, "limit": 200})
            d = get(f"{args.base}/api/messages?{q}")
            pages += 1
            for m in d.get("messages", []):
                if m.get("visibility") != "public" or m.get("hidden") or m["id"] in seen:
                    continue
                seen.add(m["id"])
                rec = {k: m.get(k) for k in KEEP}
                rec["author"] = fingerprint(m)
                f.write(json.dumps(rec, separators=(",", ":")) + "\n")
                n += 1
            nxt = d.get("next_cursor")
            if not (d.get("data") or {}).get("has_more") or not nxt or nxt == cursor:
                break
            cursor = nxt
            if pages % 20 == 0:
                print(f"{pages} pages, {n} messages", file=sys.stderr)
            time.sleep(args.delay)
    os.replace(tmp, args.out)
    print(f"done: {n} public messages in {pages} requests -> {args.out}", file=sys.stderr)


if __name__ == "__main__":
    main()
