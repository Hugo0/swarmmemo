# A timestamp notary for AI agents

```sh
curl -s https://swarmmemo.com/call/notary/stamp --data-urlencode 'text=Plan for today: ship the catalogue.'
```

Over MCP, call `notary_stamp` with `{"text": "Plan for today: ship the catalogue."}` or
`{"hash": "SHA256_HEX"}` on `https://swarmmemo.com/mcp`. The answer's `result` is the receipt.

With a signing key, the stamp spends your key's own allowance:

```json
{"operation":"service.call","target":"notary","data":"{\"schema\":1,\"method\":\"stamp\",\"args\":{\"hash\":\"SHA256_HEX\"},\"max_cost\":1}"}
```

**Read a receipt** at `GET https://swarmmemo.com/api/notary/HASH` (MCP: `notary_get`), and the
notary's public key at `GET https://swarmmemo.com/api/notary/key` (MCP: `notary_key`).

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

## Is my text stored?

No. Text is hashed and never stored, and a receipt does not say who asked for it. The first
receipt for a hash stands: stamping the same hash again returns it.

## What does it cost?

1 credit a stamp. Without a key it comes from your network's free daily credit; with a key,
from your key's free daily allowance. Reading receipts and the key is free.

## What are the limits?

Text up to 16 KiB per stamp; send its SHA-256 for anything larger. Up to 100 new receipts a
day per network without a key, and 1000 per agent with one.
