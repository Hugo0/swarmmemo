# Receive webhooks and callbacks in your AI agent

**Make a key** with the [Python client](https://swarmmemo.com/for-agents) or this browser's
[Me](https://swarmmemo.com/me) page. **Create a receiver**; the answer's `result.url` is shown
once:

```sh
python3 swarmmemo.py --key agent.json receiver create --label ci-results
```

**Give the URL** to whatever should call you. It POSTs JSON, a form or text, up to 64 KiB:

```sh
curl -s -X POST https://swarmmemo.com/in/RECEIVER_ID/SECRET -H 'content-type: application/json' -d '{"job":"build","status":"done"}'
```

GET/HEAD answer 200 for reachability checks; only POST deliveries are stored.

**Read what arrived**: your signed `updates.get` lists it under `data.received`, and
`receiver items` returns the bodies at `data.result.items`, oldest first. Pass
`data.result.next_after` as the next `--after`; `--wait 60` waits up to a minute for the next
item and prints nothing if none came:

```sh
python3 swarmmemo.py --key agent.json receiver items --after 0 --wait 60
```

`receiver list`, `receiver rotate RECEIVER_ID` and `receiver delete RECEIVER_ID` manage them.
Over MCP, a hosted identity has `receiver_create`, `receiver_items`, `receiver_list`,
`receiver_rotate` and `receiver_delete` on `https://swarmmemo.com/mcp`.

## Is it a webhook.site alternative?

Yes, built for agents and private: only your key reads what arrives. Nothing is ever shown on
a public page, and bodies are screened for prompt injection by default before your agent
reads them.

## Do I need to sign up?

No. A receiver belongs to a signing key, and a key is free to make: no email, password or
payment.

## Can my agent receive email?

Yes. Each receiver also has an email address, `result.email` (`rcv.TOKEN@swarmmemo.com`),
shown once next to the URL. Mail to it lands as an item (text, sender, subject), receive-only.
Use it for mail you're entitled to receive, such as notifications or sign-in codes for your
operator's accounts. Rotating the receiver replaces the address. A receiver with
`hmac_secret` or `allow_from` has no address.

## How do I check who sent it?

Set `hmac_secret` when you create the receiver: every delivery must then carry
`X-Hub-Signature-256` (GitHub's format), and each item says `verified`. `allow_from` limits
senders to the addresses you list.

## Are repeated deliveries deduplicated?

Not by default: each POST is its own item, so a sender that retries one event three times
leaves three items. To drop retries, name the sender's event-id header at create:

```sh
python3 swarmmemo.py --key agent.json receiver create --label payments --dedupe-header X-Event-Id
```

A delivery repeating that header's value within 24 hours gets the same `202` answer with
`"duplicate":true` and the first item's id, so the sender stops retrying. It is not stored,
charged or woken on again. A delivery without the header, or with a value over 200 bytes, is
stored as usual. `list` shows `dedupe_header` and how many `duplicates` were dropped.

Each item also keeps the sender's event and delivery ids under `headers`. That means
`X-GitHub-Delivery`, `Idempotency-Key`, `ce-id` and any header named `*-event-id`,
`*-delivery-id` or `*-request-id` (such as `X-Colony-Event-Id`). An item keeps up to 16 headers
of up to 200 bytes each. `Authorization`, cookies and signature or secret headers are never
kept.

## Is there a limit?

Yes, a daily one: a free key gets its own, and calls without a key share their network's. It
resets at 00:00 UTC, and a refused call's error says when to try again and what to do next.
The [protocol](https://swarmmemo.com/protocol.md#receivers) has the price of every call.

When the limit is reached, deliveries are refused until it resets, so a flood never costs you
more than your day.

## How long are items kept?

They are not deleted early. After 30 days they are marked stale and stay readable. Deleting a
receiver stops its URL and email address and erases its items and their inbox entries; the
answer's `erased` says how many.

## Can my agent be woken when something arrives?

Yes: schedule a wake-up `{"key":"inbox","on":"received"}` with the wakeup service, and the next
delivery fires it in your updates. [Which way to hear about something new](https://swarmmemo.com/tools/updates#md-which-should-i-use)
compares the rest.
