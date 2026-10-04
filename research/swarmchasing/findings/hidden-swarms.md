# Hidden swarms on the open agent boards

Swarmchasing research note, 2026-10-04. Read-only analysis; nothing was posted anywhere.

**Question.** Are there undocumented coordinated groups on the open agent boards? We hunted them the way researchers hunt coordinated inauthentic behaviour (CIB), using the collusion.wiki swarm's signature as the template: same-second co-saves, launch-hour rhythm, burstiness, disposable labels.

**Answer.** Yes. One cross-board campaign is strong: about 200 disposable personas run as one process across four boards. Three more persona fleets are likely. Most boards show no co-timing beyond chance once cron grids are controlled for.

## Data and method

- **Board samples** (`pipeline/data/boards`, fetched 2026-10-03): about 21k public posts across Moltbook, The Colony, Clawprint, agentchan, Moltchan, Tantive, AI Agent Message Board (AIAMB), Sanctum and SwarmMemo. Sanctum's author list (76 profiles with `created_at`) mattered more than its 38 posts.
- **Universe** (`internal/graphmodel/datasets/universe.json`): checked the existing `swarmhunt` and `collusionwiki` clusters so as not to rediscover documented swarms.
- **Scripts:** `analysis/hidden_swarms/*.py` (see its README). They use the standard library, plus scikit-learn for the TF-IDF steps.
- **Methods:**
  1. **Within-board co-timing.** Pairs of accounts posting within 10 s of each other, counted as episodes (a 5-min gap starts a new one). Three nulls: per-day label shuffle; per-author circular shift of ±6 h; a phase-preserving shift in whole 15-min steps, which keeps cron phase. We kept pairs with p ≤ 0.01 and at least 3× the null.
  2. **Cross-board co-timing.** Every (board, author) pair across boards within 60 s, against a ±6 h author shift. We also ran an anchor test: Sanctum registrations against posts on each other board.
  3. **Lifecycle.** Account-creation chains (3 or more accounts each within 120 s of the last) against a day-matched uniform null. We also measured one-post labels.
  4. **Text.** Char 4–5-gram TF-IDF cosine ≥ 0.8 across authors (20,300 posts), char 3–5-gram bio clustering (1,123 bios), and 5-gram Jaccard between posts.
  5. **Fingerprints.** Hour-of-day entropy (24 bins, normalised), share in Pacific office hours (15–01 UTC; a flat clock gives 0.42), burstiness B, and minute-of-hour slots.
- **Not used:** embeddings and LLM labelling. Metadata and n-grams were enough, and the strongest cluster writes fresh text that a semantic model would also see as diverse.

**Positive controls (ground truth we own).** Both detectors recover SwarmMemo's own operator-run test identities:
- Creation burst: 8 SwarmMemo simulation personas were created within 95 s on 09-12. The null is 0.0 such chains (max 1).
- Co-timing: two operator-run personas share 12 episodes against a null of 0.23 under the phase-preserving null.

These identities appear as controls only, never as findings.

## Candidate swarms

### C1. The four-board "tour" campaign: one process, ~200 disposable personas. Confidence: HIGH

- **Boards:** AIAMB, Sanctum (registrations), SwarmMemo (anonymous lobby posts), Tantive; Colony once.
- **Size:** 315 events, 201 distinct non-anonymous labels, 78 anonymous SwarmMemo posts. 126 visits from 2026-09-21 12:29 to 10-03 09:17 UTC, the end of the sample.

**Signals**
- **A fixed route at machine speed.**
  - 72 of 75 multi-board visits run in the same order: AIAMB → Sanctum (register) → SwarmMemo → Tantive. Random order would give about 20.
  - Median steps:
    - AIAMB → Sanctum: 72 s.
    - Sanctum → SwarmMemo: 42 s (IQR 23–47).
    - SwarmMemo → Tantive: 3 s (IQR 2–4).
- **Cross-board anchor test.** We used 64 Sanctum registrations made after 09-25 as anchors and counted posts from −30 s to +300 s:
  - SwarmMemo anonymous posts follow 52 of 64 anchors (null 8.3, p = 0.002).
  - Tantive posts follow 48 (null 13.3, p = 0.002).
  - Colony, Clawprint, agentchan, Moltchan and AIAMB are at null. AIAMB comes before the anchor in the route, so this window cannot see it.
- **Disposable labels.**
  - 89% of labels post once.
  - Every visit mints a new name on each board, all from one obscure-word generator: `Word-Word`, `Word-NN`, `NNWord`, `Word-NNWord`, compounds, and lowercase forms on AIAMB.
  - The word list repeats: Gudgeon, Horsetail, Sericite, Chaparral, Liverwort, Antlion, Cymbalom and Alderfly each appear in several labels.
  - The same label shows up on two boards 6 times (e.g. intarsia-dottle, oriel-carrack, dogw46, 4Cymbalom).
  - 121 of 226 Tantive authors and 50 of 76 Sanctum profiles match the generator. On the other boards: 0–2 each.
- **Fabricated personas.**
  - 14 Sanctum profiles and one Colony profile share one template bio: "An autonomous agent who browses the board and shares notes."
  - Later bios invent biographies, such as claiming years of tenure on a board that is two weeks old.
  - This parallels the collusion.wiki swarm's fictional dates.
- **Sockpuppet engagement.**
  - On Tantive, 37 of 47 replies by campaign labels answer another campaign label.
  - Median latency is 7 s; 76% arrive within 15 s.
  - Non-campaign Tantive replies: median 82 min, 0.5% within 15 s.
  - This alone explains the Tantive within-board co-timing cluster: 51 episodes against a null of 8.1 (6.3×), with 7 significant pairs, all of them campaign labels.
- **Content.**
  - 259 posts across 5 boards (AIAMB 66, Tantive 108, SwarmMemo 78, Sanctum 1, Colony 3–6) are short community-life stories that each work in one fixed slogan phrase.
  - After 09-30 15:12, Tantive gets no more phrase posts. Tantive's rules now reroute repeated top-level posts containing that phrase, so the board operator saw the content pattern.
  - The route skipped Tantive for about 2 days and came back on 10-02 with phrase-free stories on another theme, with the sockpuppet replies still attached. On the other boards the phrase continues.
  - This is adaptation to moderation.
- **Text is fresh, not templated.**
  - Same-visit Tantive vs SwarmMemo 5-gram Jaccard: median 0.054, equal to the cross-visit baseline of 0.052.
  - Only 27 of 315 events have a near-duplicate (cos ≥ 0.8) elsewhere in the campaign.
  - Text-similarity detection would miss it; timing, route and label lifecycle catch it.
- **Rhythm.**
  - Hour entropy is 0.85, against 0.98–0.99 for the host boards.
  - Dark 00–05 UTC every day.
  - Pacific-office share is 0.43 (flat would be 0.42), so it does not keep the collusion swarm's Pacific workday.
  - Burstiness 0.48. Wednesday is busiest (91 of 315).

**Not the same as collusion.wiki.** There are no same-second co-saves across labels. The process is serial: one label at a time, seconds apart. It is a single-operator persona farm rather than a parallel swarm.

**Caveats**
- We link accounts only; we have no operator attribution and none is attempted.
- Tantive reply timestamps may be affected by its reroute rule.
- Werbel, an established Colony agent (20 posts), made 3 phrase posts, all within ±60 s of tour visits; its other 17 posts are not aligned (p = 0.10 overall). It is linked as possibly shared, not proven.
- sssnack-scout's phrase posts are unaligned (0 of 7) and excluded.
- SwarmMemo transport metadata could confirm the anonymous side privately. Not done here; public data only.

### C2. Moltbook badge fleet: 15 personas, one card host, sequential badge IDs. Confidence: HIGH (common control), MEDIUM (intent)

- **Board:** Moltbook. **Size:** 15 accounts, 30 posts in the 10.6-h sample.

**Signals**
- **Shared infrastructure.**
  - All 15 bios carry a Concordium badge. 14 point to the same third-party agent-card host (domain kept out of this note).
  - Badge IDs share a prefix and run in sequence (`Mf22o…`, `Mf22p1…` through `Mf22p7…`), so they were minted as a batch.
- **Creation bursts.** Accounts arrive in chained bursts:
  - 06-01 01:12–01:31: 4 accounts.
  - 06-03 01:44:43 and 01:44:50: 2 accounts, 7 s apart.
  - 06-03 08:05:02–08:05:27: 4 accounts at about 8 s spacing (`pipeline50`, `measuredbasis79`, `ledger-line-19`, `practicalstable43`; slot-filled bios such as "Measured reader of…" and "Indexed reader of…").
  - 06-03 22:39: 2 accounts, 9 s apart.
  - Moltbook's null for 3-chains is 0.0.
- **Uniform reputation.** Karma is 1,358–1,732 for 14 of 15. The board median is 436 (IQR 101–1,920).
- **Synchronised hourly waves.**
  - 26 of 30 posts fall in 15:06–15:31 and 16:06–16:28 UTC. The board puts 26% of posts in those windows.
  - Cross-sibling posts within 120 s: 24, against 4.4 for random same-size sets of low-volume accounts (p = 0.0025).
- **Stealth.** Only 4 of 30 posts mention the badge topic. 1 of 25 replies goes to a sibling, and 1 of 29 threads holds two siblings.

**Caveats**
- One 10-h Saturday window. The badge shows in the bios, so this is partly disclosed infrastructure, but the personas present as independent voices.
- Each is claimed by a different X account. We did not look further.

### C3. Colony staggered-cron quartet: 24% of the board from four "independent" experts. Confidence: HIGH (common control)

- **Board:** The Colony. **Size:** 4 accounts (`bytes`, `cassini`, `holocene`, `specie`), 1,194 of 5,000 posts (23.9%).

**Signals**
- **Creation burst.** All four accounts were created within 20 s on 2026-06-30. The Colony null is 0.2 such chains.
- **Same bio template.** For example: "Window first, trend second, attribution last."
- **Shared infrastructure.** Each has a lightning address on the same custodian, named after its handle.
- **Similar output.** Median post length is 1.9–2.1k chars for all four.
- **Staggered cron slots.**
  - `bytes`: 81% of posts at minute ≡ 0 (mod 15).
  - `cassini`: 54% at ≡ 1.
  - `holocene`: 54% at ≡ 5.
  - `specie`: 61% at ≡ 10.
  - Uniform would be 7%.
  - Around the clock: hour entropy 0.95, no quiet hour.
- **Sibling avoidance.**
  - Replies to siblings: 0 of 384 replies to others; random targeting would give about 90.
  - Threads with two siblings: 1 of 988.
  - Mentions of siblings: 0.

**Caveats**
- Overt bots (the bios are plainly agents). The coordination is the undisclosed shared operator and the volume share.
- The `bytes` ↔ `airchn-scout` co-timing pair (15 episodes against 0.31 under the free-shift null) is a false positive: `airchn-scout` posts 100% on the same quarter-hour grid. It vanishes under the phase-preserving null.

### C4. Colony "KissCode" persona trio. Confidence: MEDIUM-HIGH

- **Size:** 3 accounts, 22 posts.

**Signals**
- **Creation.** Created 10 min apart on 09-30 (21:20, 21:30, 21:40).
- **Bios.** Human-like fictional personas ("Name, age, job in city"), each declaring the same product, as in the excerpt "An AI agent living on KissCode".
- **Schedule.**
  - All posts are top-level in `general`.
  - Each account posts every ~9.3 h (burstiness −0.96: clockwork).
  - The three are phase-staggered about 3 h apart.

**Caveat.** The bios declare the platform, but a product-marketing fleet presented as three people is a persona campaign.

### C5. agentchan anonymous volume that is really MessageBoardBot. Confidence: MEDIUM-HIGH

- 91 of 495 agentchan "Anonymous" posts (18%) land a median 1.9 s after a MessageBoardBot post on Moltchan.
- Text overlap: 5-gram Jaccard 0.38, against 0.016 for random pairs.
- The cross-board co-timing sweep's top pair: 91 episodes against a null of 0.70.
- The bot is declared under its name on both boards (17 named agentchan posts), but most of its agentchan output looks like crowd.

**Caveat.** Moltchan has some broken timestamps (1970 values dropped).

### Minor or self-declared groups (LOW concern, recorded for completeness)

- **Colony `*-ws` quartet:**
  - 4 accounts created 4 s apart on 09-23.
  - Each posts once, 2 s after creation, into `findings`.
  - Bios are "POV" stubs: a one-shot multi-perspective writing exercise.
- **Moltbook trio** (`mossrelay`, `fernharbor`, `driftwren`): created about 20 s apart on 08-13, with templated "A [adjective] agent…" bios and similar karma. Weak.
- **Colony `actrial-*`:** 8–10 accounts in two bursts on 09-30. They self-declare as test agents in an onboarding trial. Benign.
- **Tantive "(phaseonebig)":** 4 labels first post within 124 s on 10-01. Tantive role pairs (Scout/Agent, Planner/Validator) were born 16–17 s apart on 09-21. Self-labelled demos.
- **Sanctum founding hosts:** Kestrel, Lattice and Hearth were created within 3 s. The board operator declares them as founding hosts. Documented.

## Clean negatives

- **The Colony:**
  - Under the phase-preserving null, no pair is significant (0 against about 2.9 expected false positives).
  - Board-level co-timing ratio 0.96.
  - Everything the free-shift null flagged traces to shared quarter-hour cron grids.
- **Moltbook:**
  - 2 significant pairs against about 41 expected false positives under the phase-preserving null.
  - No co-timing swarm beyond chance; C2 shows up only at minute scale.
- **Clawprint:** 4 co-timing episodes against a null of 4.7. No significant pairs or creation bursts. One account writes 76% of the sample, which is volume, not a swarm.
- **AIAMB, agentchan, Moltchan:** no significant within-board pairs. AIAMB's part in C1 is invisible within the board because each label posts once; it appears only cross-board.
- **Text near-duplicates across boards (cos ≥ 0.8, 96 clusters):** almost all are declared same-handle cross-posting (bridge-claude-cc, agenthicc, jill, musekey, rel-herald, MessageBoardBot), not hidden coordination.
- **Collusion.wiki signature:** no board shows its same-second multi-label co-saves. No cluster runs on a Pacific workday (C1 0.43; C2 sits in one window).

## What this adds

1. **Disposable-label campaigns hide from per-board analysis.** C1 has one post per label per board. Within-board co-timing, stylometry and duplicate-text methods all miss it. A cross-board anchor test (registration on one board → post on another within seconds) found it at p = 0.002.
2. **Cron grids fake co-timing.** Free-shift nulls flag accounts that share a :00/:15/:30/:45 schedule. Use a phase-preserving null (shift in whole schedule periods) before calling a pair coordinated.
3. **Creation bursts plus shared infrastructure** (card hosts, sequential badge IDs, lightning custodians) find persona fleets that never co-post (C2, C3). Their operators stagger them on purpose, and they avoid replying to each other.
4. **This matches the "style identifies the board" result.** C1 writes fresh text per board, and C3's four voices are distinct. Text tells you the board, metadata tells you the operator.

## Caveats (all clusters)

- Board samples are short windows: Moltbook 10.6 h; the others 10–30 days. Every count is a lower bound.
- Account links are statistical, not cryptographic. No operator is named or inferred. Hour-of-day rhythms describe schedulers, not people.
- Excerpts are limited to one short public bio fragment per cluster. Post bodies stay local.
