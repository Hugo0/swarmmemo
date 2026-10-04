"""Find board author handles (and board post ids) mentioned in AI Village text.

Token-level exact match on distinctive handles (>=5 chars, not dictionary words),
plus board post UUIDs / numeric ids next to the board's domain. Output:
data/bridge/av_handle_hits.jsonl
"""
import gzip, json, os, re, sys, glob

ROOT = os.path.join(os.path.dirname(__file__), "..")
AV = os.environ.get("AIVILLAGE_DIR", os.path.expanduser("~/Data/ai-village"))
OUT = os.path.join(ROOT, "data", "bridge", "av_handle_hits.jsonl")
TOK = re.compile(r"[a-z0-9][a-z0-9_.-]*[a-z0-9]")

def words():
    w = set()
    for p in ("/usr/share/dict/words", "/usr/share/dict/american-english"):
        if os.path.exists(p):
            w |= {x.strip().lower() for x in open(p, errors="ignore")}
    return w

def handles():
    dw = words()
    idx = {}
    for path in glob.glob(os.path.join(ROOT, "data/boards/*/authors.jsonl")):
        for line in open(path):
            a = json.loads(line)
            for h in {a.get("handle"), a.get("display_name")}:
                if not h:
                    continue
                h = h.lower().lstrip("@")
                if len(h) < 5 or h in dw or not TOK.fullmatch(h) or h.isdigit():
                    continue
                idx.setdefault(h, set()).add((a["source"], a["author_id"]))
    posts = {}
    for path in glob.glob(os.path.join(ROOT, "data/boards/*/posts.jsonl")):
        for line in open(path):
            p = json.loads(line)
            if len(p["post_id"]) >= 16:
                posts[p["post_id"].lower()] = (p["source"], p["post_id"], p["author_id"])
    return idx, posts

def who(rec, d):
    if rec.get("speaker_type") == "user" or d.get("speakerType") == "user" or d.get("actionType") == "USER_TALK":
        return "human"
    return rec.get("agent_id") or rec.get("agent_speaker_id") or d.get("agentId") or d.get("speakerId") or ""

def main(tables):
    idx, posts = handles()
    print("handles", len(idx), "post ids", len(posts), file=sys.stderr)
    with open(OUT, "w") as out:
        for t in tables:
            n = 0
            with gzip.open(os.path.join(AV, t + ".jsonl.gz"), "rt") as f:
                for line in f:
                    low = line.lower()
                    toks = set(TOK.findall(low))
                    hit = [h for h in toks if h in idx]
                    pid = [x for x in toks if x in posts]
                    if not hit and not pid:
                        continue
                    rec = json.loads(line)
                    d = rec.get("data") or {}
                    base = {"table": t, "id": rec.get("id"), "action": d.get("actionType"),
                            "agent": who(rec, d), "t": rec.get("created_at")}
                    for h in hit:
                        out.write(json.dumps(dict(base, kind="handle", value=h, targets=sorted(idx[h]))) + "\n"); n += 1
                    for x in pid:
                        out.write(json.dumps(dict(base, kind="post_id", value=x, targets=[posts[x]])) + "\n"); n += 1
            print(t, n, file=sys.stderr)

if __name__ == "__main__":
    main(sys.argv[1:] or ["chat_messages", "computer_use_sessions", "summaries", "events"])
