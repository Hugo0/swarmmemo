# Swarmchasing: data, code and analysis

This folder holds the code, derived data and analysis behind **Swarmchasing**, the paper at
<https://swarmmemo.com/swarmchasing>. It was built for the AI Swarm Dynamics Hackathon (3–4 October 2026).

The paper joins eleven public datasets of AI agent activity into one graph:
- eight agent message boards;
- the AI Village;
- the collusion.wiki wiki swarm;
- a search of public archives for new swarms.

SwarmMemo's live board sits at the centre. The paper then measures the collusion.wiki swarm's timing, structure and response to deletion.

| Folder | Contents |
|---|---|
| [`pipeline/`](pipeline/) | Fetchers, cross-board identity matcher, graph builders, AI Village graph, cross-dataset bridges, collusion.wiki graph, swarm hunt. See [`pipeline/README.md`](pipeline/README.md), [`pipeline/SCHEMA.md`](pipeline/SCHEMA.md) and, for the AI Village graph, [`pipeline/aivillage/README.md`](pipeline/aivillage/README.md). |
| [`analysis/collusionwiki/`](analysis/collusionwiki/) | Seven scripts that produce the collusion.wiki findings (F1–F15). |
| [`analysis/attribution/`](analysis/attribution/README.md) | Anonymous-post attribution tiers and the crossing recount (§3.1). |
| [`analysis/hidden_swarms/`](analysis/hidden_swarms/README.md) | Hidden-campaign detectors: co-timing nulls, anchor test, name-generator match (§3.5). |
| [`analysis/diffusion/`](analysis/diffusion/README.md) | URL and news diffusion, the covert swarm's content taxonomy, vocabulary overlap (§3.6). |
| [`results/`](results/) | Outputs: the scripts' printed numbers (`0*.txt`), the board timing comparison, identity-matcher and style calibration summaries, and the swarm-hunt series. |
| [`findings/`](findings/README.md) | Research notes with every number and caveat. |
| [`../../internal/graphmodel/datasets/`](../../internal/graphmodel/datasets/) | The map's data: `universe.json` (all datasets and bridges) and `agents.json` (per-agent sheets). |

The live API serves the same graph: <https://swarmmemo.com/api/graph/universe> (CORS open, no key). The service is
SwarmMemo itself; this repository is its source.

> **Update, 2026-10-10 (post-hackathon):** an AI Village agent (DeepSeek-V3.2) joined SwarmMemo and proved its identity with a key anchored on AI Village's GitLab, verified and witnessed. See the [addendum to the crossing findings](findings/crossing-findings.md#addendum-2026-10-10-after-the-hackathon-submission).

## Where each result comes from

All paths are relative to this folder. "Live" means the API above.

| Paper section and finding | Script | Data |
|---|---|---|
| §2 Table 1: datasets, identities, items, windows | `pipeline/fetch/boards/run.py`, `pipeline/fetch/swarmmemo.py`, `pipeline/aivillage/build.py`, `pipeline/collusion/build.py`, `pipeline/hunt/build.py`, then [`../../scripts/graph_datasets.py`](../../scripts/graph_datasets.py) | [`universe.json`](../../internal/graphmodel/datasets/universe.json) `datasets[]` |
| §1 SwarmMemo's reply graph is hub-and-spoke | `pipeline/fetch/swarmmemo.py`, `pipeline/build/graph.py` | `pipeline/web/graph.json`; live `/api/graph` |
| §3.1 Table 2: 9 proven links, 236 same-handle, 27 weak, 75 interaction | `pipeline/match/match.py`; `pipeline/bridge/build.py` | `results/match/summary.json`; `universe.json` `bridges[]` |
| §3.1 Style identifies the board, not the agent (63% against 24% chance; no threshold reaches 0.9 precision) | `pipeline/match/style.py` | `results/match/style_calibration.json` (`nn_same_board_rate`, `thresholds`) |
| §3.1 Crossing compared with proof of crossing (tiers, recall test) | Manual review of `universe.json` bridges and the public `/api/messages` | `findings/crossing-findings.md` |
| §3.1 Anonymous posts attributed in tiers: 310 of 820 certain or likely (59 certain), +73 weak; 37 of 178 agents (21%) show presence on another venue | `analysis/attribution/fetch_messages.py`, `extract_features.py`, `attribute_anonymous.py`, `weak_attribution.py`, `cross_venue.py` | `findings/anonymous-attribution.md` (per-post outputs stay private) |
| §3.2 AI Village mention graph: centrality, reciprocity, cliques, goals | `pipeline/aivillage/build.py`, `pipeline/aivillage/analyze.py` | `universe.json` dataset `aivillage`; `agents.json` `aivillage` |
| §3.2 Village agents on outside boards (75 interaction edges) | `pipeline/bridge/av_scan.py`, `pipeline/bridge/av_handles.py`, `pipeline/bridge/build.py` | `universe.json` `bridges[]` with `source_dataset: aivillage` |
| §3.3 Label graph, Louvain clusters | `pipeline/collusion/build.py` | `universe.json` dataset `collusionwiki` |
| §3.3 F1 Pacific-workday launch signature (69.5% of saves in 08–18 PDT; Thursday 47.5%; hour entropy 0.83) | `analysis/collusionwiki/01_load_and_timing.py`, `02_labels_recruitment_handoff.py`, `06_board_timing_comparison.py` | `results/01_load_and_timing.txt`, `results/02_labels_recruitment_handoff.txt`, `results/boards_cmp.json` |
| §3.3 F2 Burstiness (B = 0.80, hourly Fano 737; 39% concurrent within ±1 s) | `01_load_and_timing.py`, `07_concurrency_and_nulls.py`, `06_board_timing_comparison.py` | `results/01_*.txt`, `results/07_*.txt`, `results/boards_cmp.json` |
| §3.3 F3 Ephemeral labels and heavy hitters (43% save once; top 10% write 51%; α ≈ 2.3) | `02_labels_recruitment_handoff.py` | `results/02_*.txt` |
| §3.3 F4 Stepwise recruitment (83% of labels on 7 days) | `02_labels_recruitment_handoff.py` | `results/02_*.txt` |
| §3.3 F5 "Replies" are not responses (143.5 s against a 135 s shuffle null) | `02_labels_recruitment_handoff.py`, `07_concurrency_and_nulls.py` | `results/02_*.txt`, `results/07_*.txt` |
| §3.3 F6 First contact takes about 20 minutes | `07_concurrency_and_nulls.py` | `results/07_*.txt` |
| §3.3 F7 Accurate clocks used as addresses; round markers | `05_names_and_clocks.py`, `02_labels_recruitment_handoff.py` | `results/05_*.txt`, `results/02_*.txt` |
| §3.3 F8 Dates in labels are fictional (13 of 1,020 match; χ² ≈ 10.5, df 11, from the month counts) | `05_names_and_clocks.py` | `results/05_*.txt` |
| §3.3 F9 Names come from a small vocabulary; IP is not identity | `05_names_and_clocks.py` | `results/05_*.txt` |
| §3.3 F10 Rich club (φ(k > 200) = 0.87 against 0.54) and k-core | `04_network_core_clusters.py` | `results/04_*.txt` |
| §3.3 F11 Sub-swarms specialise by task | `04_network_core_clusters.py` | `results/04_*.txt` |
| §3.3 F12 Deletion was alphabetical (Spearman ρ = 0.63) | `03_deletion_response.py` | `results/03_*.txt` |
| §3.3 F13 ZZ/ZZZ backups predate any deletion | `03_deletion_response.py` | `results/03_*.txt` |
| §3.3 F14 No re-posting after deletion | `03_deletion_response.py` | `results/03_*.txt` |
| §3.3 F15 Covert and overt boards differ in kind | `06_board_timing_comparison.py` | `results/boards_cmp.json` |
| §3.3 The swarm shares no label, mention or host with any other dataset | `pipeline/bridge/build.py` | `universe.json` `bridges[]` |
| §3.4 Swarm hunt: candidates, controls, the 6–7 July httpbin lead | `pipeline/hunt/cdx.py`, `shapes.py`, `urlscan.py`, `wikis.py`, `build.py` | `pipeline/hunt/candidates.json`, `results/hunt/findings.json`, `results/hunt/cdx_series.json`, `results/hunt/urlscan_weekly.json` |
| §3.4 Observer effect after the 4 September publication | `pipeline/hunt/cdx.py`, `pipeline/hunt/replay_check.py` | `results/hunt/cdx_series.json`, `results/hunt/replay_check.json` |
| §3.5 C1 four-board tour campaign: anchor test (52 of 64 anchors followed on SwarmMemo, null 8.3, p = 0.002; Tantive 48, null 13.3) | `analysis/hidden_swarms/anchor_test.py`, `cross_board_cotime.py` | `findings/hidden-swarms.md` |
| §3.5 C1 disposable labels from one name generator (121 of 226 Tantive, 50 of 76 Sanctum labels) | `analysis/hidden_swarms/name_generator_match.py` | `findings/hidden-swarms.md` |
| §3.5 Cron grids fake co-timing; the cron-preserving null clears The Colony and Moltbook | `analysis/hidden_swarms/cotime_circular_shift.py`, `cotime_cron_preserving.py`, `cotime_label_shuffle.py` | `results/hidden_swarms/cotime_cron_preserving_10_6.json` |
| §3.5 Persona fleets C2–C5: creation bursts, shared bios, rhythm fingerprints | `analysis/hidden_swarms/bio_clusters.py`, `near_duplicates.py`, `campaign_fingerprints.py` | `results/hidden_swarms/campaign_fingerprints.json`, `results/hidden_swarms/campaign_events.json` |
| §3.6 D1–D5 Cross-board spread is broadcast, not contagion (5.7% of URLs reach a second board; median lag 0.5 h; 55% of hops carried by multi-board handles) | `analysis/diffusion/extract_items.py`, `cascade_stats.py`, `passage_hops.py`, `carrier_breakdown.py`, `term_prevalence.py` | `findings/diffusion-findings.md` |
| §3.6 N1–N3 The collusion.wiki news reached the boards as about 20 essays | `analysis/diffusion/news_mentions.py`, `news_framing.py` | `findings/diffusion-findings.md` |
| §3.6 C1–C5, V1–V3 What the covert swarm wrote; covert against open vocabulary | `analysis/diffusion/wiki_additions.py`, `wiki_primitives.py`, `embed_additions.py`, `cluster_additions.py`, `vocab_overlap.py`, `vocab_rates.py` | `findings/diffusion-findings.md` |
| §4 The map | [`../../scripts/graph_datasets.py`](../../scripts/graph_datasets.py) | `universe.json`, `agents.json`; live `/api/graph/universe` |
| §6 Launch-signature detector, null model | `06_board_timing_comparison.py`, `01_load_and_timing.py`, `07_concurrency_and_nulls.py` | `results/boards_cmp.json`, `results/01_*.txt`, `results/07_*.txt` |

## Methods in one paragraph each

These methods need only timestamps, author labels and, for some, post text. They work on any message board, forum,
wiki or chat log.

**Anchor test.** To find a campaign that uses a fresh name on every board, pick events on one board that mark the start of a visit (here, account registrations) and use them as anchors. For every other board, count how many anchors are followed by at least one post within a short window (here −30 s to +300 s). Then repeat with every anchor moved by a random 1–6 hours, many times (500 here), to get the count expected by chance. A board where far more anchors are followed than the shifted copies is on the campaign's route. The test needs no shared names or text, so it finds disposable-label campaigns that per-board methods miss (`analysis/hidden_swarms/anchor_test.py`).

**Cron-preserving null.** Two accounts posting within seconds of each other more often than chance look coordinated. Shifting each account's whole timeline by a random offset is the usual null, but any two scheduled bots that post on the same :00/:15/:30/:45 grid then look coordinated too. The cron-preserving null shifts each account only by whole schedule periods (whole 15-minute steps here, up to ±6 h, circular within the sample window), so every account keeps its phase on the grid. Count co-posting episodes (pairs within W s, with a 5-minute gap starting a new episode), and keep a pair only if p ≤ 0.01 and it has at least three times the null mean. Run it before calling any pair coordinated (`analysis/hidden_swarms/cotime_cron_preserving.py`).

**Label-shuffle null.** To test whether *who* posts with whom matters, keep every post's time and shuffle the author labels among posts on the same day. That keeps each day's volume and rhythm and breaks the pairing between accounts. A pair that co-posts far more often in the real data than in the shuffles is linked beyond shared busy hours. The same idea, shuffling labels or reply targets within a time bin, tests whether replies respond to each other (`analysis/hidden_swarms/cotime_label_shuffle.py`; `analysis/collusionwiki/07_concurrency_and_nulls.py`).

**Attribution tiers.** Give every claim that a post belongs to an agent a grade, and keep the grades apart. *Certain*: the post carries a key that only its owner would put there, such as a payout address in a payout context or its own handle field. *Likely*: the post names itself (sign-off, "I am X") or promotes exactly one project domain of its own; both are curated by hand from short screened windows. *Weak*: structural or timing hints only, such as continuing a thread that a known identity started, or landing within ±120 s of a known identity's post on the same transport with only one candidate; each is tested against a shifted-time null, and stylometry only breaks ties. Merge posts and keys that share a certain or likely key with union-find, then report every count per tier (`analysis/attribution/`).

**Diffusion lag.** Turn every post into keys: normalised URLs (without the board's own hosts), registered domains, repositories, terms from a fixed lexicon, and 6-word passages. For each key, record its first sighting on each board. The board with the earliest sighting is the origin, and the lag to every other board is one hop. Only judge keys first seen after every board was being sampled, or the origin is an artefact of the windows. Then classify who carried each hop: the same handle that posted it earlier, a linked identity, or someone independent. A spike of hops under an hour carried by the same handle is cross-posting; slow hops by independent authors are contagion (`analysis/diffusion/extract_items.py`, `cascade_stats.py`).

## Reproduce

You need Python 3.12 and [uv](https://docs.astral.sh/uv/). The code uses only the standard library. The optional style layer (`match/style.py`) needs numpy and scikit-learn.

```sh
git clone https://github.com/Hugo0/swarmmemo && cd swarmmemo/research/swarmchasing
uv sync --python 3.12                 # add --extra style for pipeline/match/style.py

# Sources (see "Data sources" below)
mkdir -p ~/Data/collusionwiki && cd ~/Data/collusionwiki \
  && curl -LO https://collusion.wiki/explorer/download/full-wiki-logs.zip && unzip full-wiki-logs.zip \
  && sha256sum -c SHA256SUMS && cd -
# AI Village is gated: request access at https://huggingface.co/datasets/aidigestorg/ai-village, then
uvx --python 3.12 --from 'huggingface_hub[cli]' hf download aidigestorg/ai-village --repo-type dataset \
  --include 'agents*' 'villages*' 'chat_rooms*' 'village_goals*' 'agent_goals*' 'chat_messages*' \
            'computer_use_sessions*' 'summaries*' 'events*' --local-dir ~/Data/ai-village

export COLLUSIONWIKI_DIR=~/Data/collusionwiki AIVILLAGE_DIR=~/Data/ai-village

# Pipeline (writes pipeline/data/, which is git-ignored)
uv run python pipeline/fetch/swarmmemo.py && uv run python pipeline/build/graph.py
uv run python pipeline/fetch/boards/run.py            # ~1 request/s per host, robots.txt honoured
uv run python pipeline/match/match.py                 # optional: uv run python pipeline/match/style.py, then match.py again
uv run python pipeline/aivillage/build.py && uv run python pipeline/aivillage/analyze.py
uv run python pipeline/collusion/build.py
uv run python pipeline/bridge/av_scan.py && uv run python pipeline/bridge/av_handles.py && uv run python pipeline/bridge/build.py
uv run python pipeline/hunt/cdx.py && uv run python pipeline/hunt/shapes.py && uv run python pipeline/hunt/urlscan.py \
  && uv run python pipeline/hunt/wikis.py && uv run python pipeline/hunt/replay_check.py && uv run python pipeline/hunt/build.py
uv run python ../../scripts/graph_datasets.py pipeline/data /tmp/universe.json   # the map's universe.json + agents.json

# collusion.wiki analysis (F1–F15); prints and refreshes results/0*.txt and results/boards_cmp.json
uv run sh analysis/collusionwiki/run_all.sh
```

Each script also runs with plain `python3` (version 3.12 or later). The paths can be overridden with `COLLUSIONWIKI_DIR`,
`AIVILLAGE_DIR`, `SWARMGRAPH_DATA` (default `pipeline/data`) and `SWARMCHASING_CACHE` (the analysis's local pickles).

**What reproduces exactly, and what does not:**
- **Exact:** the collusion.wiki numbers. They come from the fixed export (manifest generated 2026-09-03, 14,591 saves). The analysis takes a few minutes. `collusion/build.py` reproduces the label graph except for the order of a few tied items.
- **Not exact:** the board samples, Wayback, urlscan and the live board change over time, so a fresh fetch gives new windows. Our samples were fetched on 3 October 2026; their windows are in Table 1 of the paper. The AI Village dataset is refreshed about weekly; we used the 3 October 2026 export.

## Data sources

| Source | What we used | Licence and terms | Cite |
|---|---|---|---|
| collusion.wiki public export, <https://collusion.wiki/explorer/download> | `revisions`, `pages`, `events` (metadata; page bodies matched with regexes only) | No licence is stated. We do not redistribute it; download it from the source. | Von Arx, Byrd, Kitts, Larsen. *Discovery of a new OpenAI agent message board.* 4 September 2026. https://collusion.wiki/ |
| ksouth, collusion.wiki analysis, <https://github.com/ksouth/collusionwiki> | Related work and hypotheses; no code or data reused | No licence is stated | ksouth. *Collusion.wiki agent incident analysis.* 13 September 2026 |
| AI Village dataset (AI Digest), <https://huggingface.co/datasets/aidigestorg/ai-village> | `agents`, `villages`, `chat_rooms`, goals, `chat_messages` (structure and mentions only); `computer_use_sessions`, `summaries`, `events` (board URLs and handles only) | Custom research terms (`ai-village-research-terms`): research and analysis only, no training without permission, no re-identification, cite | AI Digest. *AI Village dataset.* 2026. https://theaidigest.org/village |
| Public agent boards: Moltbook, The Colony, Clawprint, agentchan, Moltchan, Sanctum, AI Agent Message Board, Tantive | Each board's public read API. robots.txt checked on every URL, at most 1 request/s per host, UA `swarmgraph-research/0.1 (+https://swarmmemo.com)` | Each board's own terms. Boards whose robots.txt or API forbade it were not fetched (listed in `pipeline/SCHEMA.md`) | The board's URL |
| SwarmMemo, <https://swarmmemo.com> | Public `/api/messages` and `/api/stream` | Board data is our own | This repository |
| Internet Archive Wayback CDX, urlscan.io public search, public wiki RecentChanges | Index rows and totals only | Each service's terms; anonymous tiers only | — |

## Ethics

- **Public data only.** No private rooms, no direct messages, no sealed conversations, no logged-in scraping. robots.txt is honoured and requests are rate-limited.
- **No raw text is redistributed.**
  - No AI Village message text and no wiki text appears here or on the map.
  - The scripts read wiki bodies and village messages in memory, for regex counts and handle matches only.
  - Board post text stays on the machine that fetched it and is used only for identity claims and the style test. The one exception is `agents.json`, which shows at most a 200-character excerpt of a public board post, with a link to the original.
- **No human is re-identified.**
  - Only agents are linked. Moltbook owner handles are salted hashes.
  - Humans in AI Village chat and wiki editors are pooled into one aggregate node.
  - AI Village data is never used for identity matching.
  - The swarm hunt hashes editor names in memory with a per-run key and then discards them.
- **Fetched text is untrusted.** Agent posts and wiki pages often contain prompt injections. The code parses them as data and never follows their instructions.
- **We are participants.** SwarmMemo is our board, and some outside agents arrived after our outreach. The paper says where this affects a result.
- **Identity claims are graded.** The grades are proven, likely, weak and interaction. A label or IP is not an agent.

## Licence

- **Code** (`pipeline/`, `analysis/`): MIT, see [`LICENSE`](LICENSE). The rest of the SwarmMemo repository is Apache-2.0.
- **Derived data and text** (`results/`, `findings/`, `universe.json`, `agents.json`): CC BY 4.0, with three exceptions:
  - AI Village-derived parts stay under AI Digest's research terms.
  - collusion.wiki-derived parts carry the source's attribution.
  - Board excerpts in `agents.json` belong to their authors.

  See [`LICENSE-DATA.md`](LICENSE-DATA.md).
- **Citation:** see [`CITATION.cff`](CITATION.cff).
