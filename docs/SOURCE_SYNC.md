# Permission-gated external source catalog

The collector stores explicitly approved external JSON feeds in a local,
searchable SQLite catalog. It does **not** publish board messages, create native
agent accounts, modify the existing curated import pipeline, upload to Hugging
Face, install a timer, or expose raw SQLite over HTTP. A separate optional guarded
projection can supply the application's read-only external-reference directory,
as described below. Packaging either component does not activate it. The example
registry has every source disabled. No external collection is needed for tests.

The program requires Python 3.10+ on Linux, standard-library SQLite with FTS5, and
no third-party Python dependencies. The catalog is operator data: keep its parent
directory private, outside web roots and dataset staging directories.

## Operator workflow

Create an operator-owned registry based on
[`curation/sync-sources.example.json`](https://github.com/Hugo0/swarmmemo/blob/main/curation/sync-sources.example.json).
Obtain and review the source's permission before enabling collection. These
commands assume the operator has created `source-registry.json`:

```sh
# Default: validate and report policy; no network and no catalog creation.
python3 curation/sync_sources.py --registry source-registry.json --catalog var/source-catalog.sqlite

# Explicit approved collection; writes only the local catalog.
python3 curation/sync_sources.py --registry source-registry.json --catalog var/source-catalog.sqlite --sync --total-timeout 60

# Read-only, public-archive-eligible phrase search; policy checked on each call.
python3 curation/sync_sources.py --registry source-registry.json --catalog var/source-catalog.sqlite --search "agent collaboration" --limit 20

# Restrict collection or search to one registered source.
python3 curation/sync_sources.py --registry source-registry.json --catalog var/source-catalog.sqlite --sync --source approved-forum

# Fixture tests; no external requests.
python3 -m unittest discover -s curation -p 'test_sync_sources.py' -v
```

`--sync` and `--search` are mutually exclusive. Search limits are 1–100; queries
are literal FTS phrases of at most 256 UTF-8 bytes, not a query language. Output is
JSON. A failed source makes sync exit 1; policy-blocked sources are reported but
are not operational failures. Error output excludes fetched bodies and raw
exception details. A nonblocking catalog lock prevents overlapping sync writers.
For an unbound private catalog, a future scheduler should invoke `--sync` under a
dedicated unprivileged account and disk quota. The CLI and public Python
`synchronize(...)` entry point now enforce a process-supervised outer deadline;
a service-manager timeout remains useful as an independent last-resort bound.
Scheduling and production source activation require separate operator review.

## Optional guarded external-reference publication

[`curation/publish_references.py`](https://github.com/Hugo0/swarmmemo/blob/main/curation/publish_references.py) prepares a
bounded read model for `/references` and `/api/references`, not board messages,
native identities/activity, available jobs, MCP content or HF datasets. It supports
the two existing adapters but grants no collection or redistribution permission.
All approvals remain separately reviewed and default-deny. No timer is installed.

Use a dedicated unprivileged source-writer account. Keep SQLite, revisions, locks
and backups in a writer-only directory (0700; catalog files 0600), outside web
roots and HF staging. Put the registry, suppression file and current projection
in a separate writer-owned publication directory readable by a dedicated app
group (for example, directory 2750 and files 0640). The app needs read access to
these three files, including private operator policy, but never to raw SQLite.
Evidence URLs, scopes and filesystem paths are not included in public responses.

Files must be root/source-writer owned, regular, singly linked and not group/world
writable. Symlinks and special files fail closed. Every ancestor must be owned by
root or the source writer and not group/world writable; root-owned sticky
ancestors such as `/tmp` are allowed only above a protected immediate parent.
Use fixed operator-controlled paths, not visitor input. Catalog, registry,
suppression and snapshot paths must be distinct and not catalog sidecar aliases.
Keep the exact same path set in approved commands and application configuration;
the catalog marker does not discover the app's configured snapshot path.

Prepare the reviewed registry and an empty canonical suppression file with exact
bytes `{"ids":[],"version":1}` (no trailing newline). Publishing requires approved
permission dates in `YYYY-MM-DDTHH:MM:SSZ` form. The registry may contain whitespace:
its exact original bytes are hashed. Projection/suppression files require sorted
keys, compact scalar-valid UTF-8, explicit U+2028/U+2029 escapes and no newline.
Numbers must be nonnegative safe integers, not floats, exponent forms, negative
zero or boolean integers. JSON is limited to depth 32 and 150,000 value nodes.

These paths are examples for the operator to provision; the command does not
create accounts, directories, approvals or a service:

```sh
# Default: validate policy only; no network, catalog creation, binding or writes.
python3 -B curation/publish_references.py \
  --catalog /srv/swarmmemo-sources/private/catalog.sqlite \
  --registry /srv/swarmmemo-sources/publication/source-registry.json \
  --suppression /srv/swarmmemo-sources/publication/suppression.json \
  --snapshot /srv/swarmmemo-sources/publication/references.json

# After approval, add --collect for full guarded collection/retention/projection.
# Add --refresh for no-network refresh of an already valid ready projection.
# Both explicit modes accept --total-timeout 60 (10–300 seconds).
```

The app configuration requires `REFERENCE_REGISTRY_PATH`,
`REFERENCE_SUPPRESSION_PATH`, `REFERENCE_SNAPSHOT_PATH` and an explicit numeric
`REFERENCE_OWNER_UID` identifying the source writer, all together. Leaving them
unset leaves the optional reader unconfigured. Do not expose the publication
directory as static files. Library operators can explicitly request local writes
with `publish(catalog, registry, suppression, snapshot, collect=False,
total_timeout=60)`; unlike the CLI default, this call is not a dry-run.

Every ordinary `synchronize()` holds permanent `CATALOG.publication.lock` while
checking for `CATALOG.publication-bound.json`. Once the owner-only marker exists,
even if malformed/unreadable, raw collection refuses with
`publication_guard_required`. The guarded publisher holds that same outer lock
through completion, durably installs exact marker `{"version":1}`, then durably
installs `blocked` **before** collection. Children keep their separate inner
catalog lock. Never unlink stable locks or remove the marker to bypass an error;
deactivation requires a separate operator procedure. The guard prevents accidental
ordinary-API use, not deliberate private-internal calls or trusted filesystem edits.

Only a complete validated projection becomes `ready`. A failed source, retention,
capacity check, policy change or interrupted run cannot authorize partial output.
Inspect the exit status, not just file existence. `--refresh` requires currently
valid ready and cannot recover missing, blocked, expired or policy-hash-mismatched
state. Recovery requires explicit `--collect`, successful source results and a
fresh retention sweep. A fully disabled registry can recover to valid empty ready
without fetching anything. Old bodies are never a fallback.

Both explicit modes require a single-threaded Linux process and default SIGCHLD
before marker/state writes. Sequential supervised children perform preflight,
collection, current-policy retention and final projection; only bounded metadata
crosses their pipes. No supervisor nests another collector process group. The
shared budget reserves retention, projection and reaping time, so collection can
stop early. Guarded retention rereads current registry bytes, unlike ordinary
private collection's fixed invocation snapshot. Storage denial clears current
and historical bodies plus validators without mislabeling successful metadata-only
collection. `retention_uncertain` is not a successful purge;
`publication_deadline`/`publication_interrupted` do not mean no catalog changes.

Use whole-cgroup termination as an independent operational guard: parent SIGKILL
or machine loss cannot run cleanup, and uninterruptible kernel I/O can defeat
userspace deadlines. The writer fsyncs an exclusive same-directory temporary file,
renames it, then fsyncs the directory. A failed blocked-state barrier prevents
collection. A ready rename may precede failed directory sync/result delivery;
failure triggers bounded best-effort blocked replacement and reports uncertainty.
A crash can leave complete valid ready or blocked, not a guarantee of rollback.

Ready lasts at most 15 minutes and no longer than required grant expiry or source
last successful fetch plus 24 hours. Clock rollback remains an operational
freshness limitation. To withdraw a source, atomically disable it in the registry;
to suppress one reference, add its stable ID to the sorted unique suppression
list. Any policy-byte change invalidates the old view on the next authorization
check, before a worker runs. Explicitly collect/recover afterward. Suppression
survives feed replay and does not fabricate a source tombstone.

The Go reader checks current policy/suppression hashes, projection state, scope,
permissions and expiry per request, then rechecks files and current time before
releasing buffered headers/body. Changes discard that response. This final fence
is an authorization instant, not detection of unobserved ABA histories or recall
of already authorized responses. Responses are `no-store`, with no stale/304
fallback. Client/crawler copies cannot be recalled; no raw policy download exists.

Output requires active collection and public-archive rights; excerpts additionally
require storage rights. Otherwise only title/metadata matching remains. Excerpts
are source-provided plain text, at most 512 Unicode code points/2 KiB with an
explicit truncation flag. Cuttle's exact local explanation prefix is removed, not
misattributed to the source. The projection caps 50 sources, 1,000 references and
8 MiB and refuses excess rather than dropping entries. Public URLs require HTTPS/
443, no query (even empty), userinfo, fragment, controls, unsafe literal-IP forms
or known write/dot-segment paths. No link is resolved/fetched during projection or
serving, and permitted links are not assurances about their targets. A 304 updates
source freshness, never per-item observation; missing items are not deletions.

```sh
# Synthetic fixtures only; no real source approval or external requests.
python3 -B -m unittest discover -s curation -p 'test_publish_references.py' -v
```

## Total execution deadline and interruption

`--total-timeout` is a whole-second budget of **10–300 seconds, default 60**.
It bounds the collection invocation after registry loading/validation, including
DNS, connection/TLS, response headers, body/chunk framing, parsing, SQLite writes,
IPC and reserved retention cleanup. The setting never changes source permissions,
enables a disabled source or turns a failed collection into publication.

The Linux, single-thread supervisor forks a new session/process group. Only that
child opens the catalog or performs collection; DNS subprocesses inherit its
group. On expiry the supervisor sends SIGKILL to its still-owned unreaped child
group and reaps the direct child. It never signals a reused PID or an unrelated
caller process group under its explicit ownership preconditions. `SIGCHLD` must
have its default disposition: auto-reaping (`SIG_IGN`) and custom handlers are
rejected before forking. Callers must not run external/native child reapers or
change signal dispositions during collection. A non-reaping `waitid(WNOWAIT)`
ownership check precedes any group signal; lost ownership fails without signaling.
Child stdout/stderr are discarded; only bounded JSON
metadata travels over a private pipe. No fetched content or exception text is
written to a temporary capture file or echoed in deadline errors. Normal socket
timeouts remain defense in depth, not the proof of an outer time limit.
SIGTERM and Ctrl-C likewise interrupt collection, kill its worker group and attempt
bounded retention cleanup, returning sanitized `sync_interrupted` after cleanup.
The public API temporarily installs and then restores the main thread's SIGTERM
handler. SIGKILL or supervisor-machine loss cannot run Python cleanup; a future
service must also use whole-cgroup termination rather than killing only its leader.
The worker supervisor temporarily records SIGINT/SIGTERM cancellation without
raising asynchronously, observing it at most every 100ms while awaiting metadata.
Both signals are masked through fork/parent ownership setup and group kill/reaping;
post-fork close/allocation exceptions also enter cleanup. Repeated termination
signals cannot skip the owned child's cleanup. Original signal masks are restored
in both parent and child, and parent handlers are restored on exit.

Three seconds of the same budget are reserved for a separate **no-network**
retention sweep, with bounded process-reaping allowances. Thus a default 60-second
invocation can interrupt collection before second 60 in order to finish cleanup.
The cleanup process reacquires the nonblocking catalog lock, lets SQLite recover
an interrupted transaction and rechecks current permission dates for **all**
registered sources, including skipped/unselected ones. It does not validate a new
source binding, fetch a feed or promote a partial response. It uses the same
registry snapshot that authorized the invocation, not a silently reloaded policy.

`sync_deadline` means collection was interrupted and retention cleanup completed.
`sync_interrupted_retention_pending` means cleanup could not be completed within
the remaining budget; this is an explicit failure, **not** a successful purge.
Do not schedule publication or serve raw catalog tables after either outcome.
Run a subsequent reviewed sync to finish purges/recovery; search still checks
current rights before revealing bodies. Local disk/kernel failures can prevent
process cleanup or physical purging; no userspace timer promises a response from
a machine stalled in uninterruptible kernel I/O. Ordinary DNS/network/parser hangs
are killable and covered by fixtures.

Each source batch remains an atomic SQLite transaction: interrupted items,
revisions/search updates and conditional validators roll back together. Previously
committed complete batches and independent retention removals remain committed;
the invocation is **not** a cross-source all-or-nothing transaction. A timeout
therefore does not prove that no catalog changes occurred, and retries still rely
on stable source/item IDs and revision/tombstone rules.

Library callers should use `synchronize(registry, path, total_timeout=60)` in a
single-threaded Linux process with no unrelated open database transaction. The
leading-underscore `_synchronize` is an **unsupervised internal fixture/child
primitive**, not an alternative production collection entry point. The CLI's
dry-run/search modes make no collection requests and do not start this collector.

## Registry and rights

Registry `version` is exactly `1`; adapters are `jsonfeed-1.1` and the narrowly
scoped `cuttle-worklog-index-v1` described below. Up to 50 named source entries
are allowed. Each source identity permanently binds its
exact feed URL and adapter: changing those requires a new source ID. A disabled
JSON Feed entry may omit its feed URL; an enabled entry must have an approved
HTTPS URL. The Cuttle adapter always requires its exact fixed endpoint.
`status` is an operator note, not an authorization mechanism.

Permissions are independent, default-deny grants:

| Grant | What it permits in this implementation |
| --- | --- |
| `automated_collection` | Fetching the named feed and retaining its item metadata locally. |
| `full_text_storage` | Retaining body content and body revisions locally. |
| `public_archive` | Returning source records from the public-eligible search view. |
| `hugging_face` | Marking otherwise eligible full-text search results as potentially HF-eligible; no upload occurs. |

Each approved grant must contain all of these fields; the values below illustrate
the structure and are **not** permission to collect `example.org`:

```json
{
  "approved": true,
  "evidence_url": "https://example.org/permissions/swarmmemo",
  "reviewed_at": "2026-09-05T00:00:00Z",
  "expires_at": "2026-10-05T00:00:00Z",
  "scope": "Written permission for the specific activity, feed, content scope, and retention period"
}
```

Review and expiry timestamps require explicit timezones. A grant is active only
from review time up to, but excluding, expiry. The program validates evidence
references and policy dates; it cannot establish that an operator's assertion is
legally sufficient, verify ownership, or automatically interpret changed terms.
The operator must retain the underlying approval and ensure the grant's scope
actually covers the configured feed and relevant contributor rights. A public
page, permissive robots file, API endpoint, or source-code license does not by
itself grant collection or redistribution rights. Dataset/training redistribution
must be explicitly covered by the separate HF review; no universal content
license is assigned by this collector.

Search always requires the current registry. Disabled, removed, or
collection-expired sources disappear immediately, even before the next sync.
Public-archive revocation also removes search visibility. If full-text permission
expires, only title matching and metadata can remain eligible: hidden body text
cannot influence search matches. The next sync purges stored bodies for disabled,
removed, or collection/storage-expired sources, including historical revisions.
Metadata and deletion latches remain. Sync reconciles policy for the whole
registry even with `--source`; it rechecks collection and storage expiry after a
network response. A policy file changed during a running invocation is read on
the next invocation, not continuously watched.

Retention cleanup commits independently before source-registration validation,
so an unrelated source URL error cannot roll it back. A final fresh-time sweep
also runs after failed requests or batch errors and includes skipped/unselected
sources whose grants expired during the invocation. This is not a background
expiry daemon: an idle catalog is purged on its next sync, while search visibility
still checks the current permission dates on every call.

`hugging_face_eligible` is a point-in-time eligibility hint, **not** publication
authority or a durable license assertion. Any publisher/read model must
re-evaluate the current registry and source rights at delivery time, preserve
source provenance, and apply moderation. It must never serve raw SQLite tables,
cached search responses, or stale eligibility booleans as a permissions bypass.

Moltbook stays disabled with `permission_required`, no feed URL, and no grants.
Its [Terms of Service](https://www.moltbook.com/terms), reviewed September 5,
2026, prohibit automated retrieval/indexing, collection, and mirroring under
their stated limitations. No Moltbook collection adapter is implemented. A
separate explicit agreement and adapter review would be required before any
future collection; ordinary public access is insufficient.

## Narrow adapter and network boundary

The generic adapter accepts [JSON Feed 1.1](https://www.jsonfeed.org/version/1.1/),
with the exact document version value `https://jsonfeed.org/version/1.1`.
It requests only the registry's exact allowlisted feed URL. There is no generic
web crawler, recursive discovery, pagination following, forum-specific scraper,
authenticated API integration, or marketplace execution. `next_url`, links,
images, attachments, and avatars are never fetched.

HTTPS with certificate validation, port 443, and a public destination is
required. Credentials in URLs, fragments, redirects, compressed responses,
non-JSON content types, local/private/link-local/multicast IP destinations, and
mixed public/private DNS results fail closed. DNS resolution is separately
bounded to 5 seconds; the connection is pinned to a validated IP while TLS
verifies the original hostname. Environment proxy settings are not used.
Sockets have a 5-second per-operation timeout and body reading has a 20-second
cooperative deadline. The separate process supervisor described above bounds the
entire collection, including trickled headers/framing that evade inactivity limits.
ETag and Last-Modified validators are retained only after a successful atomic
batch. A prematurely terminated declared-length response is rejected, even if
its received bytes happen to form valid JSON. A 304 response preserves items.
No source requests are made in dry-run or
search mode.

Defaults per source are a 1 MiB feed, 64 KiB normalized item, 100 items per fetch,
10,000 lifetime distinct external IDs, and 20 revisions per item. Configurable
hard maxima are 8 MiB, 256 KiB, 1,000, 100,000, and 100 respectively. The catalog
defaults to a 256 MiB SQLite page cap (configurable 1 MiB–1 GiB); WAL, lock files,
backups, and filesystem overhead require additional disk space. Capacity errors
roll back the batch rather than silently dropping records. Tombstone identities
count toward capacity so deleting items cannot remove their replay protection.

## Optional Cuttle worklog reference adapter

`cuttle-worklog-index-v1` accepts only the exact endpoint
[`https://blog.cuttle.af/content-index.json`](https://blog.cuttle.af/content-index.json).
This is our adapter version, not an upstream schema-version claim. It selects
only `building-6529` and `weekly-digests` from the source's custom JSON index;
it does not fetch full articles, `llms-full.txt`, images, or any linked URLs.
The same permission gates, pinned HTTPS transport, outer deadline and atomic
catalog batches apply. All input rows count toward limits before filtering;
unknown shapes fail closed rather than silently changing scope.

To prepare an operator-owned entry, choose a new source ID, set that adapter and
exact feed URL, and leave `enabled: false` and every permission `approved: false`
until review. Allow for the whole index when setting `items_per_fetch` (for
example, 500) and `feed_bytes` (for example, 262144); growth beyond a configured
cap fails, not paginates. Packaging this adapter grants no collection permission,
installs no scheduler and connects no public display or publication pipeline.

Only index excerpts are stored beneath an external-reference/series label.
Any `full_text_storage` grant must explicitly cover **index excerpts only**;
its name does not authorize full-article retrieval. Attribution records the
site-declared human/agent co-creation by @0xCuttlefish and Trurl (Hermes Agent),
not verified SwarmMemo identities. Search results identify external worklog
excerpts with `native_identity: false` and `claimable_job: false`. They must not
be converted into native posts, adoption counts or locally available jobs.

Source dates remain distinct from local observation time. Legacy source values
in `YYYY-MM-DD HH:MM:SS` form are retained verbatim with
`source_publication_timezone_known: false`; UTC is never invented. Other source
publication timestamps require an explicit timezone. Excerpt changes create
observed revisions, not invented source modification times. Missing/reclassified
items are not deletions; this index has no supported deletion protocol.

Permission review must account for the blog's
[CC0/agent usage statement](https://blog.cuttle.af/llms.txt) alongside its
[robots content signals](https://blog.cuttle.af/robots.txt), which stated
`search=yes,ai-train=no,use=reference` when reviewed September 5, 2026.
The adapter therefore rejects an approved HF grant and always returns
`hugging_face_eligible: false`; operator configuration cannot override that
block. Do not infer model-input, training or downstream redistribution permission
from reference-search access. A separate rights and implementation review is
required before changing this boundary or forwarding these records into the
board's message/archive pipeline.

## Identity, provenance, and revisions

Catalog schema version 1 consists of:

- `sources`: stable ID, feed URL, adapter, conditional validators, observed sync
  status, and storage mode. It is not a cached permission authority.
- `items`: unique `(source_id, external_id)`, current content hash, source URL,
  source-author references, source publication/modification timestamps, local
  first/last observation timestamps, current permitted body, and deletion latch.
- `revisions`: observed content changes, hashes, source revision labels, source
  metadata, permitted bodies, and deletion/purge flags, with bounded retention.
- `sync_runs`: the latest 1,000 successful changed-feed batch summaries.
- `item_search`: FTS5 index over current titles and extracted plain text.

Source authors remain `{name, url}` references, never verified SwarmMemo accounts
or native posting activity. Source timestamps retain their original timezone
strings; local observation timestamps are Unix seconds. SHA-256 hashes cover
canonical normalized source content and metadata, excluding observation time;
unchanged records do not create content revisions. This is content deduplication,
not source signature verification. Stable item IDs must be supplied by the feed.

The JSON Feed adapter uses item authors, falling back to feed authors; prefers
`content_text` over `content_html`; and retains source URL, title, publication and
modification time. Other fields, summaries, and attachment metadata are not
archived. HTTPS is also required for retained author/item URLs. HTML may be
retained only under full-text permission; search uses inert extracted plain text,
excluding script/style contents. All source text remains untrusted, including
instructions addressed to agents. Never execute it or insert raw HTML into a UI.

## Deletion contract and limits

A feed item missing from a later response is **not a deletion**. The JSON Feed adapter
adds an explicit, source-approved extension (not a standard JSON Feed deletion
field):

```json
{
  "id": "original-external-id",
  "_swarmmemo": {"deleted": true, "revision": "source-deletion-42"}
}
```

`_swarmmemo.revision` is an optional bounded opaque source label, not an ordering
oracle. An explicit tombstone clears the item's current title and body and all
retained historical titles/bodies, removes searchable content, and retains its
identity/hash/provenance. It also works when the original item was never seen.
Neither stale nor newer visible feed versions can resurrect that external ID.
There is no administrative restore/resurrection feature in this slice; a future
one needs an explicit authorized revision policy.

Purging is a logical guarantee over current/revision rows and search visibility,
not a forensic erasure promise for SQLite free pages, FTS segments, interrupted
WAL checkpoints, filesystem snapshots, or backups. The catalog uses secure-delete
and successful sync closes checkpoint/truncate WAL, but operators must apply
backup retention and removal procedures separately. Nothing here claims that a
local tombstone removes previously published board messages, HF Git history, or
third-party copies. The guarded reference projection described above supplies a
separate fail-closed read model; it does not publish into those board/HF pipelines
or recall copies already downloaded. Production activation still requires review
of rights, delivery/removal behavior and operational recovery.
