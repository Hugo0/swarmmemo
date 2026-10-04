# AI Village interaction graph

Builds `data/aivillage/graph.json` (gitignored) from the AI Village dataset in `~/Data/ai-village/`,
in the same compact parallel-array format as `web/graph.json`. Stdlib only.

    nice -n 19 python3 aivillage/build.py     # ~7 s, streams chat_messages
    nice -n 19 python3 aivillage/analyze.py   # centrality, cliques, per-goal / per-quarter structure

## Method

- Inputs: `agents`, `villages`, `chat_rooms`, `village_goals`, `agent_goals`, `chat_messages`.
  `agent_memories` (2.4 GB) is not read.
- Nodes (`nodes.kind`): 0 agent (name, model, village, first/last message, message count, rooms, joined,
  still participating), 1 one aggregated `humans` node, 2 chat room, 3 village, 4 village goal (with season),
  5 per-agent assigned goal.
- Agent-to-agent edges. The chat has no reply field, so two signals are used and kept apart:
  - `mention`: the speaker writes `@<agent name>` (full name, or without a leading "Claude "; the longest
    alias wins). This is explicit and directed, so it's the main interaction signal.
  - `adjacent`: the speaker posts within 120 s after a different speaker in the same room (src responds to dst).
    It catches implicit turn-taking but is noisy when many agents post at once.
- Other edges: `member` (agent to room), `goal` (agent to the village goal active when it posted), `village`, `role`
  (agent to its assigned individual goal).
- Every edge has `w`, `first`, `last` (unix seconds) and `weeks`: `[[week_index, count], ...]` from `meta.t0`,
  so a replay can slice by week. `goals[]` gives goal windows and seasons (calendar quarters, UTC).
- No message text is stored. Human chat participants appear only as the aggregated `humans` node. Mentions of
  human handles are ignored.

## Counts (export of 2026-10-03)

183,485 chat messages. 148 nodes: 46 agents, 1 humans, 16 rooms, 1 village, 51 village goals and 33 agent goals.
Edges: 960 mention (total weight 53,572), 1,162 adjacent (147,821), 134 member, 607 goal, 46 village, 33 role.

## Terms and citation

The data may be used for research and analysis only. It must not be used for training or fine-tuning, and no one may
try to re-identify individuals. Never redistribute the raw data. The graph and data stay in gitignored
`data/aivillage/`. This dataset must never be joined with the cross-board identity matcher in `match/`.

> AI Digest, "AI Village dataset", 2026. https://theaidigest.org/village
