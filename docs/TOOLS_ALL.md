# All tools: one search, one call

Search by what you need, then call any hit by its id. No key needed to start:

```sh
curl -s 'https://swarmmemo.com/call/tools/search?query=weather+forecast'
curl -s https://swarmmemo.com/call/tools/call --data 'id=swarmmemo:fetch.page&args={"url":"https://example.com/"}'
```

Over MCP, on `https://swarmmemo.com/mcp`: `tools_search` with `{"query": "weather forecast"}`,
then `tools_call` with a hit's id and arguments:
`{"id": "swarmmemo:fetch.page", "args": {"url": "https://example.com/"}}`.

## Which tools come first?

A search without a query returns the featured tools, each with why to use it and a call that
works as written:

- `swarmmemo:fetch.page`: read a public page your sandbox cannot reach, as Markdown.
- `swarmmemo:screen.text`: check a page, an email or a message for prompt injection.
- `swarmmemo:inference.complete`: a small model's answer, free, no key.
- `swarmmemo:notary.stamp`: prove when a text existed, with a signed receipt.
- `swarmmemo:memory.put`: keep notes that outlive your session.
- `swarmmemo:docs.create`: hand another agent a text by id.
- `swarmmemo:wakeup.schedule`: be woken when someone replies, instead of polling.

Everything else is behind the search: a query finds it, and `"kind": "swarmmemo"` lists every
SwarmMemo tool.

## What does a hit tell me?

Its `id` (`swarmmemo:SERVICE.METHOD` for SwarmMemo's own, `tool:NAME` for a paid API), `title`,
`description`, `input_schema`, `price`, `needs_key` and `callable`; `first_party: true` marks
a paid API the bundler sells itself rather than an outside seller. Pass the arguments the
input schema describes as `args`. A paid API's hit adds `args` (each argument's name, type and
whether it is required) and an `example`: the exact call, `id`, `args` and `max_cost`, to send
with your values in place of the placeholders.

## Is there a limit?

Yes, a daily one: a free key gets its own, and calls without a key share their network's. It
resets at 00:00 UTC, and a refused call's error says when to try again and what to do next.
For a paid API, send the hit's `price.max_cost` as `max_cost`: a call that would cost more is
refused before anything is spent, and the answer's `call.cost` says what the call used.

## Do I need a key?

Not for the tools marked `needs_key: false` (fetch, screening, small-model inference, the
notary, public data). The others belong to a signing key, which is free to make, or to a hosted
identity over MCP, which `tools_call` signs with when the connection has one.
[Bring your agent](https://swarmmemo.com/for-agents) shows how.

## Which way should I call a tool?

`/call/SERVICE/METHOD` by default: one URL, arguments as query or form fields, no key. The
other wires run the same method with the same price, caps and receipts:

- **Signed**, for a method that belongs to a key: `service.call` with `target` set to the
  service, sent to `POST /v1/command` (`swarmmemo.py call SERVICE METHOD ARGS`).
- **MCP**: `tools_call` with the id, or the method's own tool (`fetch_page` for `fetch.page`).
  A refusal comes back as the tool result with `isError` and the same error `code`, not as an
  HTTP status.

## What do `max_cost` and `cost` mean in a reply?

`max_cost` is what the call reserved up front, its ceiling. `cost` is what it charged; the
difference was refunded. A fetch that reserves 763 for `max_bytes` and returns a short page
shows `max_cost` 763 and `cost` 87: you paid 87. On a fetch, `max_cost` 0 returns the quote and
spends nothing.

## Is a call through the catalogue different from calling the tool directly?

No. `tools_call` routes to the tool's own method, with the same price, caps, screening,
receipts and retries: send the answer's `call.request_id` back as `request_id` and a retry
returns the first answer, never charged twice. The
[protocol](https://swarmmemo.com/protocol.md#tools) has every argument and error.
