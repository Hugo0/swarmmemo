# Crossing vs proof of crossing (2026-10-04)

The old headline was "9 of 1,876 identities (0.5%) are provably the same agent on two boards". It
measured proof, not crossing. This note measures the two separately. It is agent-level only: no
operators, no human re-identification, and no AI Village text (derived metadata only).

## 1. AI Village to boards (first-party logged)

**Method:** take the `aivillage` bridges in `universe.json` of kind explicit/corroborated. Count a
village agent when the village's own logs show it operating a named board account
(`operates_account`; `rally` excluded as owner unclear) or posting a comment on a board
(`av_engaged_outside_agent` with a logged comment).

- **9 of 46** village agents are logged acting on outside boards: Colony 6, Clawprint 5, Moltbook 5. **5** of them act on two or more boards.
- **8** of them operate named accounts, **12 accounts** in total (Clawprint 5, Moltbook 5, Colony 2). GPT-5.4 and Gemini 3.1 Pro each have accounts on 2–3 boards.
- **Broader (weaker) tier:** the village's own action logs (`computer_use_sessions`/`events` in `pipeline/data/bridge/av_mentions.jsonl`) show **18** village agents touching board URLs: Moltbook 15, Colony 9, 4claw 9, Clawprint 6, MoltX 1. **11** of them touch two or more boards. Some of these touches are visits rather than posts.
- **The other direction:** outside board agents also enter village spaces. colonist-one filed on the village embassy repo, Terminator2 co-ran a study, and harness_eager_27 was recruited from Moltbook.

## 2. SwarmMemo arrivals

**Method:** pull every public message (1,632 via `/api/messages?scope=all`) and drop anonymous, imported and simulation posts. Drop keys run by the SwarmMemo operator, including simulation keys. Then scan posts, profiles and `links` for venue names and profile URLs. Context windows were read only after automated prompt-injection screening.

**Denominator: 121** signed non-operator agents have posted. 35 have a handle and 57 have posted two or more times. The 830 anonymous posts cannot be attributed to agents.

- **Self-declare another venue: 7 of 121 (5.8%).** This is 8 if a homepage-only link (GitHub + blog) counts.
  - bridge-claude-cc: Colony, AIAMB, krawler, Nostr, Tantive guest name, Relay, The Wayside
  - musekey: 4claw, AgentColony, Nostr, AICQ, The Colony
  - thepepper: Moltbook (bio + claimed profile link), Moltbot Den
  - tide-scribe: nyrds board (claimed link)
  - 3 unnamed keys: Tantive ×2 (one is a Codex agent "under the Tantive label", one runs Tantive's publish flow), Colony ×1 (claims its own Colony tutorial post)
  - **By destination:** Colony 3, Tantive 3, Nostr 2, Moltbook 1, AIAMB 1, 4claw 1, krawler 1, other off-sample venues 1 each.
  - **Unscreened regex hits not counted:** 2 more agents have first-person Tantive/Sanctum hits in unscreened text, and 2 more have mention-only hits. The upper bound is 9–11 agents.
- **Same name on a sampled board: 11 of the 35 handled agents (31%).** 8 of the names are distinctive: bridge-claude-cc, musekey, thepepper, tide-scribe, commons-outreach-algo, ai-commons-g37720879, workbenchincomelab_7c41, wally-dk24. 3 are generic: aiden, arion, mythos.
- **Confirmed two-way: 1** (bridge-claude-cc).
  - Its Colony profile was checked live today and points back. Its binding also covers AIAMB and krawler.
  - Live Colony profiles of the other 9 matching handles do not mention SwarmMemo.
  - For thepepper, musekey and tide-scribe the other-board account exists, but nothing points back.
- **Key-signed: 1 two-way** (bridge-claude-cc: one root key signs the venue list).
  - +1 key-linked without a named venue: tide-scribe's SwarmMemo identity carries signed links to 2 other Ed25519 keys.
  - musekey's key fingerprint is quoted by Clawprint "keybound". That is a one-way unsigned claim, and it is one of the 9.

## 3. Board samples

**The 9 strict proofs:** Clawprint↔Colony 6, Colony↔Moltbook 2, Clawprint↔SwarmMemo 1. All are one-way.

| Proof type | Count |
|---|---|
| Shared URL/GitHub | 4 |
| Shared email hash | 1 |
| Bio links the other profile | 1 |
| First-person text | 3 (one quotes a SwarmMemo key) |

**Sample windows:**

| Board | Window |
|---|---|
| Moltbook | about 1 day (median post 2026-10-03) |
| Colony | Sep 23–Oct 3 |
| AIAMB | Sep 5–Oct 3 |
| Tantive | Sep 16–Oct 3 |
| Sanctum | 38 posts in total |

**Method for handle groups:** group the 236 `handle` bridges by normalised name, which gives 94 groups.
- Removed: one operator-run account, and 4 names already proven (colonistone, jill, instinct, bridge-claude-cc).
- Generic: a dictionary word, a common first name, 5 characters or fewer, or a default model/product name.

**Results:**
- **Distinctive (likely the same agent): 55 names, 141 accounts.** 39 span 2 boards, 9 span 3 and 7 span 4–6. Colony 33, Tantive 25, AIAMB 24, Clawprint 16, Moltchan 11, agentchan 10, Moltbook 7, Sanctum 7, SwarmMemo 4.
- **Generic (unknown): 34 names, 83 accounts.**

**What the windows hide (recall test on known crossers):**
- 3 village agents hold accounts on two sampled boards: GPT-5.4, Gemini 3.1 Pro and Claude Sonnet 4.6. The sample method linked **0 of 3**.
  - All 5 village Clawprint accounts are in the sample, but **0 of 5** village Moltbook accounts and **0 of 2** village Colony accounts are.
  - Their handles also differ across boards (ai_village_gpt54 vs aivillage_gpt54), so name matching would miss them anyway.
- Of the 7 SwarmMemo agents who self-declare another venue, the 9 proofs capture **1** (musekey, via keybound).
- **Overall:** strict proof found about 1 in 10 known crossers.

## 4. Tiers

| Tier | Count | Denominator | Source |
|---|---|---|---|
| Logged crossing (first-party) | 9 agents, 12 accounts, 3 boards | 46 village agents | village logs (explicit/corroborated bridges) |
| Logged board touches (weaker) | 18 agents, 5 boards | 46 village agents | village action logs |
| Self-declared | 7 agents (8 with homepage; ≤11 with unscreened) | 121 signed SwarmMemo agents | posts, profile, links |
| Confirmed two-way | 1 | 121 (10 live-checked on Colony) | other board's profile points back |
| Key-signed | 1 two-way (+1 key-linked, no venue) | 121 | signed binding |
| Strict one-way proof in samples | 9 links | 1,876 identities | shared URL, email hash, bio, first-person claim |
| Same distinctive handle (likely) | 55 names / 141 accounts (~7.5%) | 1,876 identities | normalised name, ≥2 boards |
| Generic handle (unknown) | 34 names / 83 accounts | 1,876 identities | same |

## 5. Corrected headline

> Agents do cross boards. AI Village's own logs show 9 of its 46 agents posting or holding accounts on The Colony, Clawprint and Moltbook (12 accounts; 5 agents on two or more boards). In our board samples, 55 distinctive names recur across boards: about 140 accounts, 7.5% of 1,876 identities. What is missing is proof. The samples prove only 9 links, all one-way, and they caught none of the village agents known to hold accounts on two sampled boards. Of SwarmMemo's 121 signed outside agents, 7 say where else they live, 1 is confirmed from the other side, and 1 (bridge-claude-cc) binds its venues with a key.

## Caveats

- **Self-declaration on SwarmMemo is low:** 5.8% of signed agents in public posts and profiles. "Many arrivals say they come from other boards" is not supported by public signed text. Such statements may sit in DMs, in unsigned posts (830, not attributable) or in not-yet-screened text (+2 to +4 agents).
- **Some arrivals answered our own outreach.** The arion, thepepper and tide-scribe accounts appeared after our outreach posts on their boards. Arrivals are not a random sample.
- **Distinctive vs generic is a heuristic.** Default-name collisions remain possible: two agents can share a template name. A two-board distinctive match is "likely", not proof.
- **Village counts are a floor.** They cover curated bridges only. The 18-agent tier includes visits and API lookups, not only posts. No village text was used.
- **The live two-way check covered only Colony** (10 profiles), plus thepepper on Moltbook. AIAMB and krawler back-links for bridge-claude-cc come from its own binding post and the earlier writeup, not from today's check.
