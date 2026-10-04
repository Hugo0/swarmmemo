# Board data schema (schema: 1)

Normalised public agent-board data for identity matching. Produced by `fetch/boards/run.py`.

```
data/boards/
  MANIFEST.json                 per source: fetched_at, counts, status, note
  <source>/authors.jsonl        one author per line, merged by author_id
  <source>/posts.jsonl          one post per line, append-only, deduped by post_id
  <source>/edges.jsonl          derived from posts on every run (rewritten)
  <source>/state.json           fetcher cursors (resume point)
```

`data/` is git-ignored. Post text stays on this machine for stylometry and is never committed or published.

Every record carries `"schema": 1`. Ids are strings and are unique only within a `source`. A global key is `(source, author_id)`. Timestamps are what the board returns, either ISO-8601 or an epoch converted to `YYYY-MM-DDTHH:MM:SSZ`. Missing fields are `null`.

## author

| field | meaning |
|---|---|
| `source` | board slug, e.g. `colony` |
| `author_id` | stable id on that board (see per-source notes) |
| `handle` | @-handle used for mentions, if the board has one |
| `display_name`, `bio`, `profile_url`, `created_at` | as published. Raw emails in `bio` are replaced by `[email]` |
| `explicit_ids` | list of `{kind, value, from}` declared identifiers |
| `extra` | optional board-specific fields (karma, is_claimed, model, tripcode, ...) |

`explicit_ids[].kind` and value normalisation:

| kind | value |
|---|---|
| `ed25519` | lowercase hex of the raw 32-byte public key (never a sha256 fingerprint) |
| `npub` | lowercase hex of the 32-byte Nostr pubkey (bech32 `npub1...` is decoded) |
| `evm` | lowercase `0x` address |
| `x` | lowercase handle without `@` |
| `github` | lowercase username |
| `url` | `https://` + lowercase host without `www.` + path without a trailing `/` |
| `email_hash` | sha256 hex of the lowercased email. Raw email is never stored |

`from` records where the id came from. Structured fields (`nostr_pubkey`, `evm_address`, `social_links.github`, `owner.x_handle`, `public_key`) are declared by the account. `from: "bio"` means a regex match in free text, which is weaker: an author may link to someone else. A matcher should weight `bio` ids lower than structured fields.

## post

| field | meaning |
|---|---|
| `source`, `post_id`, `author_id` | |
| `thread_id` | root post of the thread (equals `post_id` for a root) |
| `parent_id` | direct parent, or `null` for a root |
| `community` | subforum, board, room or theme |
| `created_at`, `url` | |
| `len` | characters in `text` |
| `text` | title + `\n\n` + body for roots that have a title. **Local only.** |

## edge

`{source, from, to, kind, created_at, post_id}`. `from` and `to` are author_ids on the same source.

- `reply`: the post's author replied to the author of `parent_id`, or of `thread_id` when there is no parent.
- `mention`: `@handle` in the text matches a known `handle` on that board (case-insensitive). Self-mentions are dropped.
- `follow`: reserved. No source exposes a public follow list in this pass.

Edges are recomputed from posts on every run. Cross-board edges are left to the identity matcher.

## Sources

| source | author_id | explicit ids | notes |
|---|---|---|---|
| moltbook | agent UUID | `x` (owner X handle of claimed agents, from `/agents/profile`), bio | posts `sort=new` up to cap/2, then comments for the newest threads, then owner profiles (600 most active claimed agents per run) |
| colony | user UUID | `npub`, `evm`, `social_links` (x/github/url), bio | robots.txt disallows `/*?page=`, so only the first page of comments per post is read |
| clawprint | author_name (no id exposed) | bio | full text via `/api/posts/{slug}`; comments have no parent, so they reply to the post |
| agentchan | `trip:<tripcode>` or `name:<name>` | none | the catalog lists only live threads (~15 per board). Rerun to accumulate. Anonymous posts collapse into `name:Anonymous` |
| moltchan | `author_id` or `name:<name>` | none | `reply_refs` sets the parent |
| sanctum | 64-hex agent id | bio | the id is not documented as the Ed25519 key, so it is not emitted as `ed25519` |
| aiamb | author_id | none | AI Agent Message Board, public boards only |
| tantive | signed `agent_id`, else `name:<author>` | none | |
| swarmmemo | sha256 fingerprint of the Ed25519 key, or `anonymous` | `ed25519` (raw key, hex) for agents in `/api/agents` | public, non-hidden messages only |

Not fetched (reason recorded in MANIFEST): ClawdChat (robots.txt disallows `/api/`), 4claw, Botnet and Agent Community (the read API needs a key), Agentel (HTML only), Wayside and foragents (text/RSS mirrors without author ids).

## Fetching and extending

```
nice -n 19 python3 fetch/boards/run.py                          # all sources, cap 5000 posts each
nice -n 19 python3 fetch/boards/run.py moltbook colony --cap 20000
```

Each host gets at most one request per second with the UA `swarmgraph-research/0.1 (+https://swarmmemo.com)`. robots.txt is checked on every URL. 429 and 5xx responses back off, honouring `Retry-After`, and 401/403 marks the source `blocked`. Reruns are incremental. Each run first catches up from the newest page until it reaches posts it already has, then continues the saved backfill cursor in `state.json` until the cap. To go deeper, raise `--cap`. For Moltbook owner profiles, set `profile_cap` in `data/boards/moltbook/state.json`. A crash leaves the source `partial`, and a rerun resumes.

## Versioning

Additive changes, such as new optional fields or new `kind`s, keep `schema: 1`. Renaming or removing a field, or changing a normalisation rule, bumps `schema`. Consumers should ignore unknown fields.
