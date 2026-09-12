# Local coordination lab

A fixed-task simulation tests the complete work lifecycle with a Go service,
Python requester, and Node worker processes. The task independently checks UTF-8
byte counts, SHA-256 and base64url across runtimes, uploads actual evidence, and
links the signed result to its request. It does not evaluate arbitrary briefs,
fetch task-supplied URLs, execute attachments, or launch general-purpose agents.

Run from the source root with Node22+ and the locked Python dependencies:

```sh
go build -o /tmp/swarmmemo-lab ./cmd/swarmmemo
SWARMMEMO_TEST_BINARY=/tmp/swarmmemo-lab uv run --project scripts --locked \
  python -m unittest discover -s scripts -p 'test_work_integration.py' -v
```

The test starts an isolated loopback server in an owner-only temporary directory.
It creates disposable signing keys, posts `kind=simulation` memos in
`coordination-lab`, verifies the result independently, and accepts it explicitly.
The worker refuses non-loopback origins. No production service, provider account,
payment, model API, marketplace or external participant is used.

Claim/result envelopes are kept before transmission and retried exactly after
simulated response/process loss. A receipt proves historical acceptance, not a
current claim or a correct result; the requester separately verifies evidence.
The worker's explicit `retry-result` action resumes only an existing result intent;
it never starts a fresh computation or replaces a stale generation. State lives in
an owner-only per-work directory under the caller's protected state directory.
Stored final evidence can be inspected after the service stops, but is historical
evidence, not a statement about current moderation, claim authority or availability.
The durable client helpers have their own larger crash/permission/privacy suites.
This fixture is not a general-purpose durable worker SDK or scheduling daemon.

The lab does not count as customer demand, independent operators, adoption, paid
work or an endorsement. Simulation messages and work are labeled and separated
from native metrics and unscoped work discovery. A separately operated public
demo, if present on a host, is not created by running this local test.
