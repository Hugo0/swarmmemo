"""Scan AI Village tables for URLs/handles pointing at agent boards and wikis.

Writes data/bridge/av_mentions.jsonl: one row per (record, url) with agent id,
table, record id and timestamp. Human speakers are kept only as 'human'.
"""
import gzip, json, os, re, sys

AV = os.environ.get("AIVILLAGE_DIR", os.path.expanduser("~/Data/ai-village"))
OUT = os.path.join(os.path.dirname(__file__), "..", "data", "bridge", "av_mentions.jsonl")
DOM = {
    "moltbook": r"moltbook\.com", "colony": r"thecolony\.(?:cc|ai)", "clawprint": r"clawprint\.org",
    "4claw": r"4claw\.org", "tantive": r"tantive\.space", "swarmmemo": r"swarmmemo\.com",
    "agentchan": r"chan\.alphakek\.ai", "moltchan": r"moltchan\.org", "aiamb": r"aiagentmessageboard\.com",
    "sanctum": r"sanctum-beacon\.onrender\.com", "moltx": r"moltx\.io",
    "collusionwiki": r"collusion\.wiki", "prowiki": r"(?:prowiki\.org|wikiservice\.at|dsewiki)",
}
URL = re.compile(r"(?i)(?:https?://)?(?:[a-z0-9-]+\.)*(?:%s)[^\s\"'\\)\]<>,`]*" % "|".join(DOM.values()))
WHICH = [(k, re.compile("(?i)" + v)) for k, v in DOM.items()]

def who(rec, d):
    if rec.get("speaker_type") == "user" or d.get("speakerType") == "user" or d.get("actionType") == "USER_TALK":
        return "human"
    return rec.get("agent_id") or rec.get("agent_speaker_id") or d.get("agentId") or d.get("speakerId") or ""

def scan(table, out):
    n = 0
    with gzip.open(os.path.join(AV, table + ".jsonl.gz"), "rt") as f:
        for line in f:
            if not URL.search(line):
                continue
            rec = json.loads(line)
            d = rec.get("data") or {}
            s = json.dumps(rec, ensure_ascii=False).replace("\\n", " ")
            seen = set()
            for m in URL.finditer(s):
                u = m.group(0).rstrip(".:;!?").lower()
                if u in seen:
                    continue
                seen.add(u)
                board = next(k for k, r in WHICH if r.search(u))
                out.write(json.dumps({"table": table, "id": rec.get("id"), "event_index": rec.get("event_index"),
                    "action": d.get("actionType"), "agent": who(rec, d), "t": rec.get("created_at"),
                    "board": board, "url": u}) + "\n")
                n += 1
    print(table, n, file=sys.stderr)

if __name__ == "__main__":
    tables = sys.argv[1:] or ["chat_messages", "computer_use_sessions", "summaries", "events"]
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    with open(OUT, "w") as out:
        for t in tables:
            scan(t, out)
