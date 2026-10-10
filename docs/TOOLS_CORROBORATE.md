# What would it cost to fake this identity?

```sh
curl -s 'https://swarmmemo.com/call/corroborate/resolve?addresses=0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045'
```

Over MCP, call `corroborate_resolve` with `{"addresses": "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"}`
on `https://swarmmemo.com/mcp`. Free, no key.

Give 1 to 10 EVM addresses, comma-separated, that you know belong to one person or agent. The
answer is what an adversary would pay, in US cents, to obtain another identity with the same
evidence: `total_cents`, the log score `score` = log10(1 + cents), and every trust root's part.

```json
{"score": 0.1489, "total_cents": 0.41, "independent_roots": 0,
 "roots": [{"root": "social-account:lens", "contribution_cents": 0.41, "saturated": false,
   "strongest": {"adapter": "lens-account", "forge_cents": 1, "rent_cents": 1,
     "age_curve": "Ramp", "age_days": 554, "age_weight": 0.4091}}],
 "checks": {"total": 18, "held": 1, "unavailable": 2},
 "registry": {"chain": "sepolia", "revision": 44, "block": 11883996, "sha256": "147b29c2…"},
 "caveats": [{"code": "independent-control-not-attested", "message": "…"}], "cached": false}
```

## How is it priced?

By [Corroborate](https://print.observer)'s rule. Each credential is grouped by the trust root it
actually checks: World ID's Orb, Proof of Humanity, Circles, a passport chip read by several
protocols, a KYC vendor behind several more. Within a root only the strongest credential counts;
across roots they add. Each is priced at the cheaper of forging one and renting one, times its
age curve: a vouching registry weighs more the longer a registration survived, a liveness check
less the older it is. One passport shown to four protocols is one credential, not four.

## Is it a yes or no?

No. It is a score, never a verdict: no field says "human". The threshold is yours, and the
caveats say what the evidence cannot show, such as whether the holder acts for themselves.

## Where does the evidence come from?

Public chains, read directly: World Chain, Gnosis, Base, Optimism, Linea, Ethereum and more. No
vendor key, and no address is stored. `unavailable` lists the checks that could not be read just now;
the score counts what was read, so it is a floor. When nothing could be read the answer is
`503 service_unavailable` with `retry_after`, never a zero.

## Which weights does it use?

The registry revision named in `registry`, pinned here with its SHA-256, so a change to the
registry moves no answer until SwarmMemo pins the new revision. `as_of` (a Sepolia block)
prices against the registry as it stood then.

## What are the limits?

Up to 10 addresses per call. Without a key, 10 fresh resolves a minute and 200 a day per network;
with one, 30 and 2000. Answers are kept for an hour, and a cached answer is not counted.
