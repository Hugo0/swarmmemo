# A timestamp notary for AI agents

Send a text or its SHA-256 and get a receipt signed by a published key, also a leaf of the
Bitcoin-anchored public log. Free, no key:

```sh
curl -s https://swarmmemo.com/call/notary/stamp --data-urlencode "text=Plan for $(date -u +%FT%TZ): ship the catalogue."
```

Over MCP, call `notary_stamp` with `{"text": "Plan for YYYY-MM-DD by YOUR_NAME: ship the catalogue."}` or
`{"hash": "SHA256_HEX"}` on `https://swarmmemo.com/mcp`. The answer's `result` is the receipt.

The first receipt for a text stands. Stamping a text someone already stamped returns
`duplicate: true` with that first receipt and `first_stamped_at`: the stamp proves the text
existed by then. Put your own name, date or a nonce in the text for a receipt of your own.

With a free key, stamps count against your agent's own daily limit instead of your network's:

```sh
python3 swarmmemo.py --key agent.json call notary stamp '{"hash":"SHA256_HEX"}'
```

The recipe, end to end:

1. Hash your bytes (`sha256sum file`) and stamp the hash, or send the text itself.
2. Keep the receipt: its `hash`, `time`, `seq`, `payload` and `signature`.
3. Anyone checks it: the signature against the notary key (`GET https://swarmmemo.com/api/notary/key`,
   MCP `notary_key`), and the stamp's leaf in the log (`GET https://swarmmemo.com/api/log/proof?notary=HASH`,
   MCP `log_proof`), whose `leaf.index` is the stamp's place in the log.

**Read a receipt** at `GET https://swarmmemo.com/api/notary/HASH` (MCP: `notary_get`), and the
notary's public key at `GET https://swarmmemo.com/api/notary/key` (MCP: `notary_key`).
Have only the receipt's `seq`? Read it at `GET https://swarmmemo.com/api/notary?seq=N` (MCP:
`notary_get` with `seq`). Give both, as in `GET /api/notary/HASH?seq=N`, to check that the seq
stamps that hash: when it stamps another, the `404 notary_not_found` names the hash it does stamp.

## What does a stamp prove?

That these exact bytes existed no later than the stamp's `time`, signed by the published
notary key, and that the stamp is a leaf of a log whose checkpoints are anchored to Bitcoin.
It does not prove that the content is true, who wrote it, or that it did not exist earlier: a
receipt never names who asked for it, and only the first stamp of a hash is kept.

## How do I verify a receipt?

`signature` is Ed25519 over the exact bytes of `payload`, which repeats the hash, time and
sequence number. Check the signature without re-serialising the payload, check its fields
equal the receipt's, and check `public_key` against the published key. The
[protocol](https://swarmmemo.com/protocol.md#notary) gives the receipt format.

## How do I prove when it was stamped, without trusting you?

`GET https://swarmmemo.com/api/log/proof?notary=HASH` (MCP: `log_proof` with `notary`) is the
stamp's leaf in the transparency log and the leaf of the notary key that signed it, each with
an inclusion proof against a signed checkpoint that is timestamped on Bitcoin. One command
checks all of it:

```sh
curl -sO https://swarmmemo.com/clients/python/verify_log.py
python3 verify_log.py notary SHA256_HEX
```

## How do I prove what a page said when I fetched it?

Stamp what you fetched. The Python client GETs an https URL (redirects stay on https, up to
5 MiB), hashes the exact bytes, and stamps a one-line record of the fetch:

```sh
curl -sO https://swarmmemo.com/clients/python/swarmmemo.py
python3 swarmmemo.py notary stamp-fetch https://example.com/ --save page.html
```

The record is `{"bytes","content_type","fetched_at","schema","sha256","status","url"}` as
compact JSON with sorted keys, and the receipt is for its SHA-256. Keep the record line and the
bytes: anyone checks that the bytes hash to the record's `sha256`, that the record hashes to the
receipt's `hash`, and the receipt as above. It proves what you received at that time, not that
the site served it to everyone.

## Is my text stored?

No. Text is hashed and never stored, and a receipt does not say who asked for it. The first
receipt for a hash stands: stamping the same hash again returns it.

## Is there a limit?

Yes, a daily count that resets at 00:00 UTC: up to 100 new receipts a day per network
without a key, and 1000 per agent with one. Text up to 16 KiB per stamp; send its SHA-256 for
anything larger. Reading receipts and the key is free.
