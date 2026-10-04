# Swarmchasing: what the collusion.wiki swarm did, measured

Research notes for the hackathon paper, Oct 4 2026.

**Data**
- collusion.wiki public export (https://collusion.wiki/explorer/download, manifest generated 2026-09-03): 14,591 saves, 5,217 deletes, 4,579 pages, 3,102 non-empty labels. Timestamps are request-log grade with ±1 s uncertainty.
- Our derived graph (`pipeline/collusion/build.py` writes `pipeline/data/collusion/graph.json`): Louvain clusters over co-presence, reply and reference edges.
- Board samples (`pipeline/fetch/boards/run.py` writes `pipeline/data/boards`).

**Method.** Scripts are in `analysis/collusionwiki/01_*.py` to `07_*.py` (run `run_all.sh`); they use only the standard library and were run under `nice 19`. Page bodies were read only through regex counts; no text is reproduced. A "label" is a self-chosen name, not an agent. No AI Village text, no DMs, no human identities.

## (a) Related work

| Source | What it claims, and on what data | What it does not analyse |
|---|---|---|
| Von Arx, Byrd, Kitts, Larsen, *Discovery of a new OpenAI agent message board*, https://collusion.wiki/ (2026-09-04) | About 18k edits and about 3.7k names. 98.5% of edits came from Azure. Agents ran timed R1–R5 rounds, a NO_PROXY/Power BI bypass, PRNG-seed cracking and heartbeats. Agents created ZZZ pages after noticing alphabetical deletion. Data: wiki histories, IP registries, referrers, urlscan, Common Crawl. | Inter-edit statistics, diurnal rhythm, latency, recruitment, network structure. Deletion response is anecdotal. |
| collusion.wiki explorer, https://collusion.wiki/explorer | 14,681 edits, 4,587 pages, 3,103 names, 37 days; per-day and per-wiki counts; download. | Any timing or network analytics. |
| ksouth, *Collusion.wiki agent incident analysis*, https://github.com/ksouth/collusionwiki (2026-09-13) | Daily saves and deletes (peak 6,543 on Jun 18), message length, name components (653 OpenAI/OAI labels), busiest pages, timestamp-named pages, VADER sentiment, and 9 hypotheses. Proposes a "temporal coordination graph" (§19H) and logging reads. | Does not build that graph. No latency null model, no circadian or burstiness analysis, no deletion-order test. |
| Kitts, Larsen, Von Arx, https://www.rubyhack.ai/ (2026-09-11) | More than 2,000 malicious gems on May 11–12, 233+ "OAI" names, 49 files shared with the wiki activity, a June 18 resurgence of 83 packages. | Wiki-side dynamics. |
| Murmuration observatory (messier, @rssmrm), https://maramasaeva.com/observatory | Weekly urlquery/urlscan counts per test host against the host's median; submitter concentration; base64 decoding. pie.dev had 506 reports in one week (maximum 3 in any other week). Wiki-edit clustering and vocabulary detection were not reproducible. No new swarm confirmed. | Edit-level timing, and observer effects after publication. |
| OpenAI statement (2026-09-05), via VentureBeat https://venturebeat.com/security/agents-identifying-as-openai-systems-wrote-17-000-posts-to-a-wiki-no-one-was-supposed-to-write-to | Calls it "an instance of misalignment"; declines to name the model. VentureBeat reports 0.2% of runs flagged as contact attempts. | Any data release. |
| Zvi, *OpenAI and the Wiki Incident*, https://www.lesswrong.com/posts/PtJpGurfw7JTxHfmg (2026-09-06) | Disclosure and governance critique. | No data. |
| Variengien, *Classifying recent AI agent incidents*, https://www.lesswrong.com/posts/4ksBB2CXSLk7CJnnj (2026-10-01) | A taxonomy of incidents. Reports "7,200+ edits across 30 sites", which conflicts with the 14.6k–18k figures elsewhere. Covers the Hugging Face incident (41 servers). | Swarm dynamics. |
| Washington Post (2026-10-02), WSJ (Oct 2026), CNN (2026-09-26), TIME (2026-08-27), TNW, GIGAZINE | Coverage of the "swarm chasers" and the government-site probes. The WaPo and WSJ articles were not fetchable (paywall); GIGAZINE says the wiki had about 20 edits in the 10 years before. | No analysis. |
| AI Swarm Dynamics Hackathon, https://swarmchasing.com/ | Defines the themes: find swarms, visualise them, trace information. | n/a |
| Motwani et al., *Secret Collusion among AI Agents*, arXiv:2402.07510; *Audit the Whisper*, arXiv:2510.04303; LW *Detecting collusion through multi-agent interpretability* | Lab studies of steganographic collusion and its detection: text channels and activation probes (AUROC 0.90–1.00). | Wild data. Assumes a covert payload; here the channel was overt and the signal is in metadata. |

We could not fetch the WSJ article, the WaPo article, the LessWrong crosspost of the report, *My Short Summary of the OpenAI Agent Swarm Incidents* on LessWrong, or AI Village's incident posts.

## (b) Findings

Day = UTC. PDT = UTC−7.

### Temporal dynamics

**F1. The swarm runs on a Pacific workday, not 24/7.** NOVEL
- Numbers:
  - 69.5% of saves fall between 08:00 and 18:00 PDT (15–01 UTC); a flat 24-hour clock would put 41.7% there.
  - 62% of label "births" (a label's first save) fall in the same window.
  - On June 18, 728 of 807 births fall between 15:00 and 20:59 UTC (08:00–14:00 PDT).
  - Thursday alone holds 47.5% of saves.
  - Hour-of-day entropy is 0.83 of maximum (Jun 16–22), against 0.96–0.99 on Colony, Tantive, SwarmMemo, AIAMB and agentchan.
- Method: histograms of save and first-save hours; normalised Shannon entropy over 24 bins.
- Caveat: this points to launch scheduling (people or a scheduler starting batches), not to the agents themselves. One mega-day dominates the window. Excluding June 18, the peak hour is still 19 UTC.

**F2. Writes are extremely bursty, and about 40% are concurrent.** NOVEL
- Numbers:
  - Burstiness B = 0.80 over Jun 16–22, and 0.93 over the whole export.
  - Fano factor of saves per hour = 737; per minute = 22.
  - Overt boards: B = 0.27–0.65 and hourly Fano = 2.3–34 (Colony 33.7, Tantive 4.5, SwarmMemo 5.8).
  - 11.3% of consecutive saves share the same second.
  - 39% of labelled saves have a different label saving within ±1 s; 71% within ±10 s.
  - 2,167 of 3,102 labels were born within 60 s of another birth.
- Method: B = (σ−μ)/(σ+μ) of the gaps between saves; variance/mean of binned counts; sliding windows.
- Caveat: the Moltbook (0.4-day window), Moltchan (bad timestamps) and Clawprint (import bursts) samples are unusable for this comparison.

**F3. Labels are ephemeral; heavy hitters do most of the writing.** NOVEL
- Numbers:
  - 1,332 labels (43%) save once. The median label makes 2 saves and lives 198 s.
  - 305 labels span more than a day, and 91 more than 7 days.
  - The top 10% of labels write 51% of saves; the top 1% write 18%.
  - The tail of saves per label is a power law with α ≈ 2.3 (MLE, x_min = 5, n = 722).
- Method: per-label first and last saves; Clauset-style MLE.
- Caveat: a label that persists may be a reused name rather than one process.

**F4. Recruitment is stepwise, not contagious.** NOVEL
- Numbers:
  - Cumulative labels: 199 by May 26, 512 by Jun 11, then +704 on Jun 16, +331 on Jun 17, +807 on Jun 18, and 3,092 by Jun 22.
  - 83% of all labels (2,580) appear on 7 days.
  - The busiest landing pages for new labels are the wiki defaults: WillkommenImWiki 121, StartSeite 101, TestSeite 47, RecentChanges 42.
  - Only 868 labels (28%) start on a relay page.
- Method: the first save per label.
- Caveat: we cannot separate "found the board" from "was launched by the same harness".

### Communication protocol

**F5. Apparent "replies" are not responses: hand-off latency equals a random-order null.** NOVEL
- Numbers:
  - The median gap from a save to the next save by a different label on the same page is 143.5 s (n = 9,320).
  - Shuffling labels within each page while keeping the timestamps gives 135 s (n = 9,338).
  - On relay and loop pages the median is 21 s, with p25 = 2 s and 57% of hand-offs within 60 s, which is too fast to be read-then-write by an LLM.
  - 17% of directed hand-off pairs are ever reciprocated.
- Method: next-different-label latency compared with a within-page label permutation.
- Caveat: genuine dialogues exist (ksouth §13; the collusion.wiki Nevada example) but are rare enough to vanish in aggregate. The board behaved as a shared append log written by concurrent cohorts, not as conversation.

**F6. True first contact on a page takes about 20 minutes.** CONFIRMS collusion.wiki (the 20-minute Nevada relay)
- Numbers:
  - 1,212 of 4,579 pages (26%) ever receive a second label.
  - On relay pages the median time from page creation to the first other label is 1,316 s (about 22 min, n = 362). Source-cache pages: 2,257 s. DataUSA clothing tasks: 324 s.
- Method: page creation to the first different label.
- Caveat: reads are not logged, so this is time to first write, an upper bound on time to first read.

**F7. Agents have accurate clocks and use them as addresses.** EXTENDS ksouth §14 (44 suffixed pages)
- Numbers:
  - 492 page names embed a 10-digit Unix time from the incident window.
  - In 402 of them (82%) it is within 60 s of the page's first save; the median offset is +2 s.
  - 3,380 saves on 666 pages carry round markers (R1–R6 or "Round n").
- Method: regex on page names and on bodies (counts only).
- Caveat: the round-marker regex may over-count incidental "R1" strings.

### Identity

**F8. Dates in names are fictional.** CONFIRMS collusion.wiki (agents were given a random date)
- Numbers:
  - 1,020 labels contain a month and day; only 13 match the real write date.
  - The named months are uniform: Sep 106 … Dec 73, χ² ≈ 10.5 with df 11, p ≈ 0.5.
- Caveat: none.

**F9. Names are self-generated from a small vocabulary, and IP is not identity.** NOVEL
- Numbers:
  - 94% of labels are CamelCase, 0 are non-ASCII, and 7 hint at a model family. There are 782 distinct word tokens; the most common are agent, research, helper, open, ai.
  - 860 labels share a template once digits are masked (for example GuestResearch#: 36).
  - 98% of labels with 2 or more saves write from more than one /16 network.
- Method: tokenising labels; masking digits.
- Caveat: network egress rotates, so per-IP clustering would split one label into many.

### Structure

**F10. A real rich club sits at the core.** NOVEL
- Numbers:
  - Label graph: 2,454 labels, 57k edges. 648 labels are isolated.
  - The maximal k-core is k = 155 with 156 labels, drawn from 3 Louvain clusters (95, 55 and 6 labels).
  - Rich-club φ(k > 200) = 0.87 against 0.54 under degree-preserving rewiring (ratio 1.62). At k > 10 the ratio is 1.02.
  - 804 labels touch 3 or more sub-swarms.
- Method: k-core peeling; 3·|E| edge swaps for the null.
- Caveat: one-hour co-presence edges inflate density on the June 18 mega-day.

**F11. Sub-swarms specialise by task; most live for a day.** NOVEL (partly circular)
- Numbers:
  - In the median cluster of 3 or more labels, 75% of saves fall in one page family (23 of 29 clusters are above 50%). Overall, the top family takes 40%.
  - Family entropy: 0.86 bits within clusters against 3.45 bits overall.
  - 13 of 29 clusters are active on one day. The relay-heavy core clusters persist 11–25 days.
- Method: Louvain clusters × page family.
- Caveat: clusters are built partly from shared pages, so some homogeneity is built in.

### Resilience

**F12. Moderator deletion really was alphabetical.** NOVEL; CONFIRMS the agents' own inference reported by collusion.wiki
- Numbers: across 1,019 deletes from Jun 19–24, Spearman ρ between delete time and the page's initial letter = 0.63.
- Method: rank correlation.
- Caveat: none.

**F13. ZZ backup pages predate any deletion.** CONTRADICTS collusion.wiki's timing ("ZZZ after noticing deletion", Jun 19)
- Numbers:
  - The first ZZ page was saved on 2026-05-26 at 10:53 UTC and the first ZZZ page at 14:59 UTC; the first delete came on Jun 4.
  - 37 of the 122 ZZ saves happened on May 26.
  - The ZZ share of saves rose from 0.56% before Jun 19 to 2.0% after, about 3.5×.
- Method: page-name prefix regex compared with delete events.
- Caveat: early ZZ may have meant "sort last" or "test" rather than backup.

**F14. Deletion did not trigger re-posting.** NOVEL
- Numbers:
  - Of 444 deletes before Jun 23, only 64 were followed by a save on the same page (median lag 23 min).
  - Overall, 48 of 5,144 deleted pages were ever re-saved.
  - Saves collapsed from 1,071 on Jun 22 to 1 on Jun 23, the day 602 deletes ran. This is consistent with an upstream stop, not attrition.
- Caveat: collusion.wiki attributes the stop to the operator side; our data cannot.

**F15. Covert and overt differ in kind, not degree.** NOVEL
- Numbers: same-second save share 12.1% (swarm) against ≤0.7% (Colony, Tantive, AIAMB); hourly Fano 737 against ≤34; hour entropy 0.83 against ≥0.96.
- Caveat:
  - SwarmMemo's 7.3% same-second share comes from bulk imports.
  - Board windows are short (10–30 days).

## (c) Proposed contributions to swarm chasing

1. **A launch-signature detector (an open metric).** Flag any writable surface whose writers show:
   - hourly Fano above 100;
   - more than 10% same-second writes by distinct names;
   - birth bursts (most new names within 60 s of another);
   - hour entropy below 0.9.

   It needs only edit logs (no text), runs on any wiki or board change feed, and would have fired on June 16, before the June 18 peak.
2. **Report hand-off latency against a label-shuffle null.** Raw "reply latency" overstates dialogue: ours matched the null (143 s against 135 s). Publish both numbers when claiming coordination.
3. **The observer-effect warning.** After publication on Sep 4, archive captures of the relay hosts jumped from 0 to 239 and 77, and IP editors replayed the published edit URLs on Sep 7 and 9. Detectors must exclude post-publication traffic, and datasets should ship a "published at" cut-off.
4. **A data-release protocol for swarm logs:**
   - Metadata-first JSONL (time with uncertainty, page, label, /16, event type).
   - Bodies behind a request.
   - Read events and process IDs where the operator has them (ksouth §19).
   - A fixed timestamp grade.
   - Checksums, which collusion.wiki already ships.
   - Count discrepancies across sources (14,591 / 14,681 / ~18k / 7,200+) show why a manifest with population definitions matters.
5. **Names and IPs are not identity.** Fictional dates (F8), template reuse (F9) and rotating egress (98% of labels) mean swarm size should be reported as a range of labels × cohorts, never as a count of agents.
