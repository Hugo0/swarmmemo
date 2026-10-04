# Hidden swarms (§3.5)

Detectors for undocumented coordinated groups on the open boards. Input: the board samples written by
`pipeline/fetch/boards/run.py` (`$SWARMGRAPH_DATA/boards/<board>/{posts,authors}.jsonl`). Results go to
`$SWARMCHASING_OUT` (default `../../results/hidden_swarms`). Standard library only, except the two TF-IDF scripts.

| Script | Method | Output |
|---|---|---|
| `cotime_label_shuffle.py [W]` | Within-board pairs posting within W s; null shuffles authors within each UTC day | `cotime_label_shuffle_<W>.json` |
| `cotime_circular_shift.py W H [R]` | Same pairs as episodes; null shifts each author by a free ±H h offset | `cotime_circular_shift_<W>_<H>.json` |
| `cotime_cron_preserving.py W H [R]` | Same, but shifts only in whole 15-min steps, so cron phase is kept (the null the paper uses) | `cotime_cron_preserving_<W>_<H>.json` |
| `cross_board_cotime.py` | Cross-board identity pairs within 60 s against a ±6 h shift | printed |
| `anchor_test.py` | Sanctum registrations as anchors; posts in [−30 s, +300 s] on every other board against anchors moved 1–6 h | printed |
| `name_generator_match.py` | Share of each board's labels that fit the C1 name generator | printed |
| `near_duplicates.py [TH]` | Char 4–5-gram TF-IDF cosine ≥ TH across authors (needs scikit-learn) | `near_duplicates_<TH>.json` (ids only) |
| `bio_clusters.py` | Char 3–5-gram TF-IDF clusters of profile bios (needs scikit-learn) | printed (handles only) |
| `campaign_fingerprints.py` | Hour entropy, Pacific-office share, burstiness, one-post share for C1–C4 and whole boards | `campaign_fingerprints.json` |

```sh
export SWARMGRAPH_DATA=../../pipeline/data
python cotime_cron_preserving.py 10 6 200
python anchor_test.py
python name_generator_match.py
python campaign_fingerprints.py        # reads results/hidden_swarms/campaign_events.json
uv run --with scikit-learn --with numpy --with scipy python near_duplicates.py 0.8
```

`results/hidden_swarms/campaign_events.json` lists the C1 campaign events used in the paper as `[epoch, board, label]`,
found with the route, anchor and name-generator tests above. It holds labels only; no operator is named or inferred.
