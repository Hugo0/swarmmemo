# Diffusion and the covert swarm's speech (§3.6)

How links, terms and passages travel between boards, how the collusion.wiki news reached them, and what the swarm
wrote. Inputs: the board samples (`$SWARMGRAPH_DATA/boards`), identity links (`$SWARMGRAPH_DATA/match/links.jsonl`) and
the collusion.wiki export (`$COLLUSIONWIKI_DIR`). Intermediates go to `$SWARMCHASING_CACHE` (default `./cache`,
git-ignored: it holds text-derived keys and wiki additions). Results are printed.

Run in this order:

| Script | What it does |
|---|---|
| `extract_items.py` | URL, domain, repo, term and 6-gram keys per post → `cache/items.pkl` |
| `cascade_stats.py` | Boards reached, origin board, first-sighting lags, carrier class (D1, D2, D4) → `cache/res.pkl` |
| `passage_hops.py` | Multi-board handles, origin rates, passage lags |
| `carrier_breakdown.py` | Who carries hops, by lag bucket (D3) |
| `term_prevalence.py` | Protocol vocabulary per board and the board-naming matrix (D4, D5) |
| `news_mentions.py`, `news_framing.py` | Posts naming the incident and related news (N1–N3); prints term matches, not text |
| `wiki_additions.py` | Added text per wiki revision → `cache/cw_added.jsonl` |
| `wiki_primitives.py` | Coordination primitives by regex and phase (C4, C5) |
| `embed_additions.py`, `cluster_additions.py` | MiniLM embeddings, KMeans (K=28), c-TF-IDF terms (C1, C2) |
| `vocab_overlap.py`, `vocab_rates.py` | Covert against open vocabulary and hosts (V1–V3) |

The window start for "who was first" is `DIFFUSION_WINDOW_START` (default 2026-09-23).
