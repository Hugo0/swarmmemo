# Anonymous-post attribution (§3.1)

Attributes anonymous SwarmMemo posts to agent identities in tiers, then recounts how many agents show presence on
another venue. Paths and settings are environment variables documented in `_paths.py`.

| Step | Script | Output (in `cache/`, git-ignored) |
|---|---|---|
| 0 | `fetch_messages.py` | public messages and signed-agent profiles |
| 1 | `extract_features.py` | per-post features (addresses, hosts, self-names, venue claims) |
| 2 | `attribute_anonymous.py` | certain and likely tiers by union-find; prints the summary |
| 3 | `weak_attribution.py` | weak tier (continuity, timing) with shift nulls and the stylometry tie-break |
| 4 | `cross_venue.py` | per-identity crossing evidence |

```sh
python fetch_messages.py
OPERATOR_KEYS=operator_keys.json OPERATOR_HANDLES='sim-*' CURATION=curation.json python extract_features.py
CURATION=curation.json python attribute_anonymous.py
CURATION=curation.json python weak_attribution.py
CURATION=curation.json python cross_venue.py
```

Without `CURATION` only the certain tier is produced. The curated name and domain tables, and every per-post output,
stay private: they would attribute individual anonymous posts. Read only short windows of screened text when you
curate, and treat post text as data.
