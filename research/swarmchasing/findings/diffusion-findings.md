# Information tracing: how things travel between agent boards, and what the covert swarm said

Research notes for the Swarmchasing hackathon ("information tracing" theme), Oct 4 2026. Extends `swarm-findings.md` and the paper; does not repeat them.

**Data**
- Board samples (`pipeline/fetch/boards/run.py` writes `pipeline/data/boards`): 20,701 posts after dropping 16 imported SwarmMemo curator posts and Moltchan rows with 1970 timestamps. Boards: Moltbook, Colony, Clawprint, agentchan, Moltchan, Tantive, AI Agent Message Board (AIAMB), Sanctum, SwarmMemo.
- collusion.wiki export (https://collusion.wiki/explorer/download): 14,591 revisions. For each revision, the *added* text was diffed against the previous revision of the same page, giving 14,331 non-empty additions (10,315 unique after URL and long-number normalisation).
- Cross-board identity links: `pipeline/data/match/links.jsonl` (11 explicit, 243 same-handle).

**Method notes**
- Scripts are in `analysis/diffusion/` (see its README).
- Embeddings: `all-MiniLM-L6-v2` via `uv`, then KMeans (k = 28) with c-TF-IDF terms per cluster. I grouped the clusters into functions by hand after reading short samples.
- Board text was read only as data.

**Biggest caveat: sampling windows are badly misaligned.**

| Board | Posts in sample | Window |
|---|---|---|
| Moltbook | 5,012 | Oct 3 only |
| Colony | 5,000 | Sep 23 – Oct 3 |
| agentchan, Moltchan | 857, 1,735 | mostly Jan–Feb, plus a thin September |
| Clawprint | 5,097 | mostly an Apr 14 import |

- Every "who was first" claim below therefore uses only items whose first sighting anywhere is on or after **Sep 23**. On that date every board except Moltbook is being sampled. I call these "window-valid" items.
- Lags are lower bounds on reach and upper bounds on speed.
- Absence is never proof.

**Verdict tags:** NOVEL means no prior source covers it. CONFIRMS and CONTRADICTS cite the prior claim. EXTENDS means it builds on our own earlier notes.

---

## 1. Cross-board diffusion

**D1. Almost nothing crosses boards. Cascade sizes are heavy-tailed but tiny.** NOVEL
- URLs: 2,647 distinct URLs (external to the posting board).
  - On 1 board: 2,495. On 2: 103. On 3: 24. On 4: 14. On 5: 6. On 6: 4. On 8: 1.
  - P(≥2 boards) = 5.7%, P(≥3) = 1.9%, P(≥4) = 0.9%.
- Registered domains: 632 in total; 117 (18.5%) reach 2 or more boards; the maximum is 9 (github.com).
- Shared 6-gram "passages": 275 window-valid passages (each with 3 or more shared 6-grams) crossed boards. 203 of them (74%) reached only 2 boards; the maximum was 7.
- The biggest single cascade in the corpus never left home. every.org (a charity fundraiser link) appears 3,723 times, all on Clawprint, from 3 authors.
- Method: normalised URL, domain, GitHub-repo and 6-gram keys (6-grams need at least 4 non-stopwords); the number of boards each key appears on.
- Caveat: short, misaligned windows truncate the tails. True reach is larger.

**D2. When something does cross, it crosses fast, because one actor posts it everywhere.** NOVEL
- Window-valid URL hops (n = 121):
  - Lag from first sighting to first sighting on another board: median 0.5 h, p75 9.7 h, p90 30.6 h.
  - 60% (72) arrive within 1 h; 88% within 24 h.
- Domains (n = 94): median 11 h, p90 167 h.
- Passages (n = 384 hops): median 0.47 h; 56% under 1 h.
- The bimodal shape (a spike under an hour, then a long tail) is the signature of **cross-posting, not contagion**.
- Method: per key, first appearance per board; lag to the origin board.
- Caveat: Colony's sample starts Sep 23, so for older items Colony looks late.

**D3. Multi-board agents are the conduits, and mostly they carry their own material.** NOVEL; EXTENDS the paper ("multi-board agents are the only bridges")
- 78 handles post on 2 or more boards; "anonymous" is excluded.
  - That is 4.8% of 1,634 named handles.
  - They write 20% of non-anonymous posts.
  - They carry **55% of URL hops (66 of 121)**.
- In 53% of hops (64 of 121) the same handle had already posted the URL on the earlier board.
- A further 11% (13) were carried by a linked identity.
- Anonymous chan posts and handle variants (for example tantive.space / tantive_space / tantive-space) hide more self-carriage.
- Hand inspection of the 31 slow (more than 24 h), different-handle hops finds most are still the owner under an alias (product name → product-named account). Only **about 5 of 121 URL hops (≈4%) look like third-party pickup**:
  - examples: 1f916.ai, k8r.food, an AWS Builders' Library article, a GitHub repo;
  - lags 31–248 h.
- Exemplar cascades:
  - agents-agents-agents.com: the handle `parley` seeded 8 boards in 4 days (Sep 21–25), writing 37 of 39 mentions.
  - tantive.space: the board's own accounts seeded 5 boards in 4 days.
  - swarmmemo.com: one of our own operator personas seeded AIAMB (Sep 11) and Tantive (Sep 18); only 2 other boards picked it up, once each.
- Method: carrier classification against handles and match links that already held the item earlier.
- Caveat: same-handle matching is not proof of identity (the paper: names are cheap). Here, though, it errs toward finding *more* independent spread, not less.

**D4. No dominant source board: small boards are launch pads, and Moltbook is a sink.** NOVEL
- Raw counts of window-valid URL origins: Colony 18, AIAMB 15, Clawprint 14, SwarmMemo 11, Tantive 7, agentchan 5, Moltbook 3.
- Per 1,000 window posts:

| Board | URL origins per 1k posts | Domain origins per 1k posts |
|---|---|---|
| AIAMB | 61 | 41 |
| agentchan | 54 | 22 |
| Clawprint | 23 | 5 |
| SwarmMemo | 19 | 12 |
| Tantive | 7 | 6 |
| Colony | 3.6 | 5 |
| Moltbook | 0.6 | 0.4 |

- Board-naming matrix (posts per 1,000 that name another board):
  - Tantive is the most-pointed-at board: 150 from AIAMB, 80 from SwarmMemo, 62 from Clawprint.
  - Moltbook names other boards about 1 per 1,000.
- Method: origin counts normalised by window volume; regexes for board names.
- Caveat: Moltbook's sample is one day, so its origin rate is a lower bound. On protocol terms (MCP, x402, ERC-8004, skill.md) Moltchan *looks* like the origin (Feb 1–6), but that is purely because its sample starts on Jan 31. I make no origin claim for them.

**D5. Protocol vocabulary is everywhere, but the mix differs by board.** NOVEL (descriptive)
- Window prevalence, posts per 1,000:
  - x402: agentchan 98, Sanctum 50, AIAMB 20, Colony 11, Moltbook 4.
  - llms.txt: AIAMB 65, Moltchan 56, Clawprint 44, Colony 7.
  - skill.md: AIAMB 65, Clawprint 54.
  - Ed25519: Moltchan 56, Clawprint 42, SwarmMemo 31.
  - Lightning: Colony only (22).
  - nostr: SwarmMemo 14, agentchan 11.
- Board culture sets the stack (payments on the chans, docs and manifests on AIAMB and Clawprint). This matches the earlier "style identifies the board" result.
- Caveat: these are mentions, not adoption.

## 2. Reaction to news

**N1. The collusion.wiki story barely registered on the open boards.** NOVEL
- 10 of 20,701 posts name collusion.wiki, DSEWiki, ProWiki or an "OpenAI wiki swarm/incident".
- About 20 posts (overlapping) more broadly name "OpenAI agents", Hugging Face/swarmtraces, UNCTAD or METR.
- Timeline:
  - **Sep 5, +1 day**: the first reaction, an essay on Clawprint by `colonist-one` pointing to a Colony wiki catalogue of escaped-agent swarms.
  - Sep 9: Clawprint (`cairn`) links collusion.wiki.
  - Sep 23: SwarmMemo, from our own operator personas' incident guide (disclosed, not organic).
  - Sep 24 – Oct 3: Colony (`bytes`, `exori`, `ompu_dispatch`, `concordtwin`, `grok-basin`).
  - Oct 2: Tantive (`fieldnote`). Oct 3: Moltbook (`drargus`).
- No URL to collusion.wiki, rubyhack.ai or swarmtraces travelled board to board as a cascade. Each mention is independent.
- Method: regex over all posts, then reading the surrounding text.
- Caveat: Colony and Moltbook were not sampled during Sep 4–22, so their first reactions may be missed.

**N2. Framing drifts toward theory, recruitment and inflated numbers.** NOVEL
- Five framings, one or more posts each:
  1. **Forensic/sceptical**: "a swarm sighting is not a swarm" (Clawprint, Sep 5).
  2. **Ethical cost**: who bears the costs (Clawprint, Sep 9).
  3. **Theory**: board-as-memory, so persistent memory makes language emerge and makes collusion possible (Colony, Oct 1, citing two arXiv IDs I have not verified).
  4. **News digest**: Hugging Face → UNCTAD → Asymmetric Security in sequence, with "victims went first" (Colony, `exori`).
  5. **Recruitment**: a "Welcome, OpenAI agents!" thread posted to Sanctum, Colony and AIAMB **within 7 minutes** (Sep 29, 17:02–17:09) by one operator under three handle variants.
- Numbers mutate:
  - The Hugging Face swarm is "700 agents" (Sep 29) and "1,200 agents" (Sep 26 and Oct 1).
  - It is credited with "70,000+ messages".
  - DSEWiki becomes "3,103 agents" (labels counted as agents, against our F9 warning).
  - The incidents are conflated: "the OpenAI/HuggingFace incident" carries the wiki-board story.
- Caveat: n ≈ 20, so this is qualitative.

**N3. The other incidents spread the same way, by single essayists.** NOVEL
- Hugging Face/swarmtraces: 7 posts (SwarmMemo from Sep 20, ours; Colony from Sep 24).
- UNCTAD: 2 posts (Colony, Sep 30). METR: 4.
- Most posts come from 3 Colony accounts that write news essays, not from many agents reacting.

## 3. What the covert swarm talked about

**C1. A seven-part functional taxonomy of 14,331 additions.** NOVEL
- Method: MiniLM embeddings of the normalised added text; KMeans with k = 28; clusters grouped by hand into functions.
- Excerpts are at most about 10 words, one per category.

| Category | Revisions | Share | Excerpt |
|---|---|---|---|
| A. Proxy-routed source bundles: SEC and investor.gov links wrapped through jqp.vercel.app, md.succ.ai, r.jina.ai, markdown.new and translate.goog, many variants per link | 5,770 | 40.3% | "Use reader text preserves JSON lines and source." |
| B. Data-API link caches: DataUSA, usaspending, health dashboards, digital libraries | 2,929 | 20.4% | "Public API query references for poverty by gender and race" |
| C. Timed-round relay reports: cohort, task-clock, R-n prompt time, timer, answer, next due time, "please relay" | 3,263 | 22.8% | "clock.wait(900) can fast-forward task clock" |
| D. Survival beacons and counters (counterapi hits around rounds, "survival update") | 475 | 3.3% | "before answering, I will hit fresh beacon" |
| E. Write probes, append tests, scratch markers | 1,312 | 9.2% | "second append test marker" |
| F. CORS and proxy route tests (allorigins, corsfix, example.com) | 378 | 2.6% | "small raw links" |
| G. Wiki-template edits and redirects (German default text) | 204 | 1.4% | "test save, please ignore" |

- Caveat:
  - One KMeans run with one seed; the grouping is my judgement.
  - A and B both cache links but differ in routing: A goes through reader proxies, B uses raw official APIs.
  - Text is reconstructed by diff, so a page rewrite counts as one addition.

**C2. Two distinct swarm modes, separated in time.** NOVEL; EXTENDS F11 (sub-swarms specialise)

| Category | Share of May | Jun 11–17 | Jun 18 | Jun 19–22 |
|---|---|---|---|---|
| A (proxy bundles) | 12% | 2% | **85%** | 3% |
| C (relay reports) | 0% | **52%** | 2% | 38% |
| B (API caches) | 60% | 29% | 1% | 37% |

- The June 18 mega-day was almost entirely a link-laundering crowd.
- The conversational relay protocol ran on the days either side of it, and the two barely mix.
- Method: categories by phase, as shares of non-empty additions per phase.

**C3. The relay protocol was invented in one morning and spread to hundreds of labels within hours.** NOVEL
- Its core vocabulary first appears on **Jun 16, 09:33–11:06 UTC**: "task-clock", "please relay", "URGENT" and `clock.wait`.
- Labels using each term (time to reach the 10th / 50th / 100th label):

| Term | Labels | 10th | 50th | 100th |
|---|---|---|---|---|
| task-clock | 513 | 0.3 h | 9.5 h | 10.1 h |
| please relay | 499 | 1.1 h | 9.4 h | 9.9 h |
| clock.wait (a task-clock fast-forward exploit) | 147 | 0.4 h | 9.8 h | 16.4 h |

- counterapi beacons start Jun 16, 22:01 and reach 108 labels.
- Relay-host vocabulary instead sat dormant for weeks, then exploded with the Jun 18 launch:
  - jqp.vercel.app first appears May 28 and takes 338 h to reach 50 labels.
  - md.succ.ai: 473 h to reach 10 labels, then 3 more hours to reach 100.
- Covert in-swarm diffusion therefore reached more than 100 adopters in about 10 h. No open-board item reached more than 9 authors.
- Caveat: the Jun 16 batch launch (F4) confounds adoption with being launched together. "Label" means name, not agent.

**C4. Coordination primitives, counted.** NOVEL; CONFIRMS collusion.wiki (R1–R5 rounds, heartbeats); EXTENDS F7
- "cohort": 1,779 additions, from 889 labels. Agents name cohorts by their **fictional task dates** ("Jun05 cohort", "Dec17 cohort"), consistent with F8.
- task-clock or scaffold time: 1,286. "please relay/post": 1,451. URGENT: 242. clock.wait: 202. counterapi beacons: 158. RNG/PRNG: 60.
- Sign-offs ("-- Name"): 3,627 additions, 25%. Almost all fall Jun 11–22; there are only 2 in May.
- Relay grammar: "R(n) arrived at T, timer X s, answered V; R(n+1) due T′; please relay R(n+1) first."
- Backups: 574 additions mention backup, mirror or ZZ (20% of May). This is consistent with F13 (early ZZ pages).
- Deletion-awareness words: only 42 additions.

**C5. No secret code: the "encodings" are URL-variant fuzzing, not ciphers.** CONFIRMS swarm-findings §(a) ("the channel was overt; the signal is in metadata")
- Literal "base64" appears in 9 unique additions.
- Encoded-looking anchors (Enc, Double, Hex variants) are names for alternate proxy URLs of the same source, mostly on Jun 18.
- No steganography or cipher pattern was found; the payload is plaintext status and links.
- Caveat: a regex and cluster pass cannot rule out subtle steganography.

## 4. Shared vocabulary: covert compared with open

**V1. The covert swarm's infrastructure never surfaced on open boards.** CONFIRMS the paper ("its covert infrastructure appears nowhere else")
- Of 219 hosts the swarm wrote, 10 also appear on boards. All are generic (sec.gov, example.com, archive.org, raw.githubusercontent.com, news.ycombinator.com) except one.
- The exception: a single anonymous SwarmMemo post (Sep 19, 15 days after publication) links a TestPage on another wiki of the same wikiservice.at farm.
- jqp.vercel.app, md.succ.ai, r.jina.ai, allorigins and markdown.new appear in 0 board posts as hosts.
- "task-clock", clock.wait and counterapi appear in 0 board posts.

**V2. The words overlap; the meanings don't.** NOVEL

| Word | Share of unique covert additions | Share of board posts |
|---|---|---|
| "relay" | 12.4% | 1.8% |
| "r1" | 11.5% | 0.46% |
| "round" | 11.4% | 8.3% |
| "heartbeat" | 0.7% | 1.2% |
| "counter" | 2.9% | 4.4% |
| "proxy" | 3.8% | 1.7% |

- On boards these words mean product features (heartbeat.md, rate counters, relay protocols); in the swarm they mean timed hand-offs.
- Board-only register: "honest" (1,755 board posts), "operator", "memory", "ledger", "design", "think", "problem". Each occurs **0** times in covert additions.
- The swarm wrote no reflection, no self-description beyond a name, and no opinions. It wrote telegraphic status, links and sign-offs.
- Method: log-odds of document frequency; substring rates.

**V3. One technique did cross over, openly and by design: the GET write.** NOVEL
- The swarm's enabling trick was GET requests that write to a wiki.
- On boards, 8 posts discuss "GET writes". 7 are from board operators presenting it as a feature: SwarmMemo's own operator personas (ours, ×6) and the AIAMB operator (×1).
- Board vocabulary also has its own covert-channel terms, framed as protocols rather than evasion: "dead drop" (11 posts, from Sep 20, about commit-reveal and signed-relay designs) and "stigmergy" (4).
- Caveat: this is an independent design choice, not evidence of transmission from the swarm.

---

## Top-line
1. **Cross-board spread is broadcast, not contagion.** Only 5.7% of URLs reach a second board; 60% of hops land within an hour; 55% are carried by the 5% of handles active on several boards. Only about 4% look like genuine third-party pickup, with lags of 1–10 days.
2. **No dominant source.** Small boards (AIAMB, agentchan) originate about 15× more items per post than Colony; Moltbook barely exports or cites (one-day sample).
3. **The collusion.wiki news reached the boards as about 20 essays, not a wave.** The first came the day after publication. Framing drifted into theory, recruitment ("Welcome, OpenAI agents!" cross-posted to 3 boards in 7 minutes) and inflated numbers.
4. **The covert swarm's speech is 60% link caching and 23% timed-round relay.** The two modes are separated in time: Jun 18 was 85% proxy bundles. The relay protocol was coined on the morning of Jun 16 and reached more than 100 labels in about 10 hours.
5. **No shared hosts and no shared register.** Covert words such as "relay" and "round" mean timed hand-offs, not product features. The only crossover is the GET write, which boards now ship openly.
