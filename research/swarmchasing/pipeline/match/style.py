#!/usr/bin/env python3
"""Optional, gated style layer: char n-gram TF-IDF author profiles, calibrated on explicit-link pairs.

Run after match.py (needs data/match/clusters.json), then rerun match.py to fold edges in.
Needs numpy + scikit-learn (an offline venv is enough). No embedding model is used: none was cached locally.

Writes data/match/style_edges.json ({meta, edges}) and data/match/style_calibration.json. Edges are emitted only
at a threshold whose measured precision >= 0.9 on known pairs; otherwise the edge list is empty.
No text is written anywhere.
"""
import json, os, random, re, sys
from collections import defaultdict

os.environ.setdefault("OMP_NUM_THREADS", "8"); os.environ.setdefault("OPENBLAS_NUM_THREADS", "8")
import numpy as np
from sklearn.feature_extraction.text import TfidfVectorizer

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
BOARDS = os.path.join(ROOT, "data", "boards")
OUT = os.path.join(ROOT, "data", "match")
MIN_POSTS, MAX_CHARS = 5, 20000
URL = re.compile(r"https?://\S+|\b0x[0-9a-fA-F]{40}\b|\b[0-9a-f]{32,}\b")
MENTION = re.compile(r"@[\w\-]+")


def main():
    cl = json.load(open(os.path.join(OUT, "clusters.json")))
    texts = defaultdict(list)
    for src in sorted(os.listdir(BOARDS)):
        p = os.path.join(BOARDS, src, "posts.jsonl")
        if not os.path.exists(p): continue
        for l in open(p):
            d = json.loads(l)
            if d["author_id"] in ("anonymous", "name:Anonymous"): continue
            t = d.get("text") or ""
            if t:
                texts[(src, d["author_id"])].append(MENTION.sub("@x", URL.sub(" ", t)))
    keys = sorted(k for k, v in texts.items() if len(v) >= MIN_POSTS)
    idx = {k: i for i, k in enumerate(keys)}
    docs = ["\n".join(texts[k])[:MAX_CHARS] for k in keys]
    vec = TfidfVectorizer(analyzer="char_wb", ngram_range=(2, 4), min_df=3, max_df=0.9, sublinear_tf=True, max_features=200000)
    X = vec.fit_transform(docs)                    # rows L2-normalised
    S = (X @ X.T).toarray(); np.fill_diagonal(S, -1)

    # split-half self-similarity (same author, two disjoint halves): an optimistic upper bound on style signal
    halfA, halfB = [], []
    for k in keys:
        ps = texts[k]; halfA.append("\n".join(ps[0::2])[:MAX_CHARS // 2]); halfB.append("\n".join(ps[1::2])[:MAX_CHARS // 2])
    HA, HB = vec.transform(halfA), vec.transform(halfB)
    H = (HA @ HB.T).toarray()
    top1 = float(np.mean(np.argmax(H, axis=1) == np.arange(len(keys))))

    pos = set()
    for a, b in cl["explicit_pairs"]:
        a, b = tuple(a), tuple(b)
        if a in idx and b in idx: pos.add((min(idx[a], idx[b]), max(idx[a], idx[b])))
    owner = set()
    for a, b in cl["same_owner_pairs"]:
        a, b = tuple(a), tuple(b)
        if a in idx and b in idx: owner.add((min(idx[a], idx[b]), max(idx[a], idx[b])))
    board = np.array([k[0] for k in keys])
    iu = np.triu_indices(len(keys), 1)
    cross = board[iu[0]] != board[iu[1]]
    sims = S[iu]
    lab = np.zeros(len(sims), bool)
    pos_ix = {p: n for n, p in enumerate(zip(iu[0].tolist(), iu[1].tolist())) if p in pos}
    for n in pos_ix.values(): lab[n] = True

    rng = random.Random(7)
    def stats(mask):
        v = sims[mask]; return {"n": int(mask.sum()), "mean": round(float(v.mean()), 4) if len(v) else None}
    same_board = ~cross
    rows = []
    # precision over ALL cross-board pairs (unlabelled = negative; conservative) and vs 100 random negatives per positive
    cand = sims[cross]; clab = lab[cross]
    neg_rand = np.array(rng.sample(list(cand[~clab]), min(len(cand[~clab]), 100 * max(1, clab.sum()))))
    for th in (0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9):
        pred = cand >= th; tp = int((pred & clab).sum())
        rows.append({"threshold": th, "predicted_all_cross": int(pred.sum()), "tp": tp,
                     "precision_all_cross": round(tp / pred.sum(), 4) if pred.sum() else None,
                     "recall": round(tp / clab.sum(), 4) if clab.sum() else None,
                     "precision_vs_100x_random": round(tp / (tp + int((neg_rand >= th).sum())), 4) if tp else None})
    ok = [r for r in rows if r["precision_all_cross"] is not None and r["precision_all_cross"] >= 0.9 and r["tp"] >= 3]
    chosen = min((r["threshold"] for r in ok), default=None)

    # does style cluster by board/template? nearest neighbour board agreement vs chance
    nn = np.argmax(S, axis=1)
    nn_same_board = float(np.mean(board[nn] == board))
    chance = float(sum((board == b).sum() * ((board == b).sum() - 1) for b in set(board)) / (len(keys) * (len(keys) - 1)))
    per_board = {b: round(float(np.mean(board[nn][board == b] == b)), 3) for b in sorted(set(board))}

    edges = []
    if chosen is not None:
        for (i, j) in zip(*np.where(np.triu(S >= chosen, 1))):
            if board[i] != board[j]:
                edges.append({"a": list(keys[i]), "b": list(keys[j]), "cos": float(S[i, j]),
                              "conf": max(r["precision_all_cross"] for r in rows if r["threshold"] <= S[i, j])})
    cal = {"authors_profiled": len(keys), "min_posts": MIN_POSTS, "features": X.shape[1],
           "positives_explicit_pairs_profiled": len(pos), "same_owner_pairs_profiled": len(owner),
           "positive_cos": sorted(round(float(S[a, b]), 3) for a, b in pos),
           "positive_rank_of_partner": sorted(int((S[a] > S[a, b]).sum()) + 1 for a, b in pos),
           "thresholds": rows, "chosen_threshold": chosen, "edges_emitted": len(edges),
           "split_half_top1": round(top1, 4), "split_half_self_cos_mean": round(float(np.mean(np.diag(H))), 4),
           "pair_means": {"same_board": stats(same_board), "cross_board": stats(cross), "explicit_positive": stats(lab)},
           "nn_same_board_rate": round(nn_same_board, 4), "nn_same_board_chance": round(chance, 4), "nn_same_board_by_board": per_board,
           "embedding_model": "none (no small sentence-embedding model in local caches; nothing downloaded)"}
    json.dump(cal, open(os.path.join(OUT, "style_calibration.json"), "w"), indent=1)
    json.dump({"meta": {k: cal[k] for k in ("authors_profiled", "positives_explicit_pairs_profiled", "chosen_threshold", "edges_emitted")},
               "edges": edges}, open(os.path.join(OUT, "style_edges.json"), "w"))
    print(json.dumps(cal, indent=1), file=sys.stderr)


if __name__ == "__main__":
    main()
