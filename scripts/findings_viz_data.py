#!/usr/bin/env python3
"""Precompute the small aggregate series behind the /swarmchasing explainer figures.

Reads the collusion.wiki public export (revisions.jsonl) and writes
internal/web/assets/findings-data.json: derived numbers only (counts, shares,
gaps). No page text, labels, page names or IPs leave this script.

    python3 scripts/findings_viz_data.py [~/Data/collusionwiki] [out.json] [campaign_events.json]

campaign_events.json is the hidden-swarms analysis's event list for the
four-board "tour" ([unix time, board, label] rows; swarmmemo-hq
research/hidden-swarms.md, C1). Only per-board counts are taken from it; when
it is absent the tour falls back to the counts published in that note.

To add a figure's data: write a function that takes the context dict and
returns a small JSON-able dict, and add it to SERIES under the key the figure
names in FindingsViz.register(id, {data: key, ...}) (assets/findings-viz.js).
"""
import json
import os
import sys
from datetime import datetime, timedelta, timezone

PDT = timezone(timedelta(hours=-7))


def load_saves(root):
    """Every save as (utc datetime, label or None). Bodies are never kept."""
    saves = []
    with open(os.path.join(root, "revisions.jsonl"), encoding="utf-8") as f:
        for line in f:
            r = json.loads(line)
            t = r.get("time")
            if not t:
                continue
            when = datetime.strptime(t, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
            saves.append((when, r.get("label") or None))
    saves.sort(key=lambda s: s[0])
    return saves


def office_hours(ctx):
    """F1: saves per hour of day (PDT) and per weekday (UTC day, Mon=0)."""
    saves = ctx["saves"]()
    hours = [0] * 24
    days = [0] * 7
    for t, _ in saves:
        hours[t.astimezone(PDT).hour] += 1
        days[t.weekday()] += 1
    n = len(saves)
    return {
        "n": n,
        "hours_pdt": hours,
        "weekday_utc": days,
        "office_share": round(sum(hours[8:18]) / n, 4),
        "uniform_share": round(10 / 24, 4),
        "thursday_share": round(days[3] / n, 4),
    }


TOUR_BOARDS = ["aiamb", "sanctum-reg", "swarmmemo", "tantive"]


def tour(ctx):
    """C1 (hidden-swarms.md): the four-board persona campaign. Route statistics
    come from the note; per-board event counts are recounted when the event
    list is available. No labels are emitted."""
    out = {
        "boards": ["AIAMB", "Sanctum", "SwarmMemo (anon)", "Tantive"],
        "step_median_s": [72, 42, 3],
        "visits_in_order": 72,
        "multi_board_visits": 75,
        "in_order_by_chance": 20,
        "labels": 201,
        "events": 315,
        "one_post_label_share": 0.89,
        "ban": "2026-09-30",
        "back": "2026-10-02",
        "per_board_events": [66, 59, 78, 108],
    }
    path = ctx.get("campaign")
    if path and os.path.exists(path):
        with open(path, encoding="utf-8") as f:
            rows = json.load(f)
        counts = {b: 0 for b in TOUR_BOARDS}
        for _, board, _ in rows:
            if board in counts:
                counts[board] += 1
        out["per_board_events"] = [counts[b] for b in TOUR_BOARDS]
    return out


def crossing(_ctx):
    """Cross-board identity tiers (anonymous-attribution.md, crossing-findings.md).
    Counts of SwarmMemo agents; no names."""
    return {
        "agents": 178,
        "signed": 117,
        "anon_only": 61,
        "evidence": {"signed": 11, "anon_only": 26},
        "self_declared": {"signed": 7, "anon_only": 4},
        "two_way": 1,
        "key_bound": 1,
        "village": {"outside": 9, "agents": 46},
    }


# One entry per figure: key (named by the figure's `data`) -> function(ctx) -> dict.
SERIES = {
    "officeHours": office_hours,
    "tour": tour,
    "crossing": crossing,
    # Next findings plug in here.
}


def main():
    root = os.path.expanduser(sys.argv[1] if len(sys.argv) > 1 else "~/Data/collusionwiki")
    here = os.path.dirname(os.path.abspath(__file__))
    default_out = os.path.join(here, "..", "internal", "web", "assets", "findings-data.json")
    out = sys.argv[2] if len(sys.argv) > 2 else default_out
    cache = {}

    def saves():
        if "saves" not in cache:
            cache["saves"] = load_saves(root)
        return cache["saves"]

    campaign = os.path.expanduser(sys.argv[3]) if len(sys.argv) > 3 else None
    data = {
        "source": "collusion.wiki public export and swarmmemo-hq research notes; aggregates only",
        "series": {k: fn({"saves": saves, "campaign": campaign}) for k, fn in SERIES.items()},
    }
    with open(out, "w", encoding="utf-8") as f:
        json.dump(data, f, separators=(",", ":"), sort_keys=True)
        f.write("\n")
    print(out, os.path.getsize(out), "bytes")


if __name__ == "__main__":
    main()
