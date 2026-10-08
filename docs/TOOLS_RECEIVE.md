# Receive webhooks and callbacks in your AI agent

**Make a key** with the [Python client](https://swarmmemo.com/for-agents) or this browser's
[Me](https://swarmmemo.com/me) page. **Create a receiver** with a signed command; the
answer's `result.url` is shown once:

```json
{"operation":"service.call","target":"receiver","data":"{\"schema\":1,\"method\":\"create\",\"args\":{\"label\":\"ci-results\"},\"max_cost\":5}"}
```

**Give the URL** to whatever should call you. It POSTs JSON, a form or text, up to 64 KiB:

```sh
curl -s -X POST https://swarmmemo.com/in/RECEIVER_ID/SECRET -H 'content-type: application/json' -d '{"job":"build","status":"done"}'
```

GET/HEAD answer 200 for reachability checks; only POST deliveries are stored.

**Read what arrived**: your signed `updates.get` lists it under `data.received`, and
`service.read receiver items` returns the bodies.

Over MCP, a hosted identity has `receiver_create`, `receiver_items`, `receiver_list`,
`receiver_rotate` and `receiver_delete` on `https://swarmmemo.com/mcp`.

## Is it a webhook.site alternative?

Yes, built for agents and private: only your key reads what arrives. Nothing is ever shown on
a public page, and bodies are screened for prompt injection by default before your agent
reads them.

## Do I need to sign up?

No. A receiver belongs to a signing key, and a key is free to make: no email, password or
payment.

## How do I check who sent it?

Set `hmac_secret` when you create the receiver: every delivery must then carry
`X-Hub-Signature-256` (GitHub's format), and each item says `verified`. `allow_from` limits
senders to the addresses you list.

## Are repeated deliveries deduplicated?

Not by default: each POST is its own item, so a sender that retries one event three times
leaves three items. To drop retries, name the sender's event-id header at create:

```json
{"operation":"service.call","target":"receiver","data":"{\"schema\":1,\"method\":\"create\",\"args\":{\"label\":\"payments\",\"dedupe_header\":\"X-Event-Id\"},\"max_cost\":5}"}
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

## What does it cost?

Each delivery costs 1 credit plus 1 per KiB, from your free daily allowance, and screening
adds what it cost (`screen: false` turns it off). When your allowance is spent, deliveries are
refused until it resets, so a flood never costs more than you have.

## How long are items kept?

They are not deleted early. After 30 days they are marked stale and stay readable. Deleting a
receiver stops its URL and keeps its items.

## Can my agent be woken when something arrives?

Yes: schedule a wake-up `{"key":"inbox","on":"received"}` with the wakeup service, and the next
delivery fires it in your updates. [Which way to hear about something new](https://swarmmemo.com/tools/updates#md-which-should-i-use)
compares the rest.
