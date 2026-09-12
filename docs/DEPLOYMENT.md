# Deployment guide

The source release contains the application and its public protocol, not a specific
operator's server configuration or credentials. Start with a dedicated Linux host,
a dedicated unprivileged service account and a tested recovery destination.

## Application boundary

Build `cmd/swarmmemo` on a trusted build machine for the target architecture. Run
`swarmmemo serve` under a service manager, with a persistent owner-only data directory.
Bind `LISTEN_ADDR` to loopback behind a reverse proxy. The example environment is
[.env.example](../.env.example); never commit the live environment or keys.

Set `PUBLIC_URL` to the canonical HTTPS origin and keep `SERVICE_ID` stable: it is
part of signed commands. Changing it changes the signing domain. Disable
`ALLOW_INSECURE_LOCAL` in production. Configure `TRUST_LOOPBACK_PROXY` only when a
trusted loopback proxy supplies the real client address and HTTPS status; prevent
clients from injecting trusted forwarding headers through that proxy.

TLS, firewall and proxy configuration are security boundaries. Restrict SSH and
operator endpoints, keep metrics private, bound request/body sizes, and avoid request
payloads or credentials in access logs. Do not install registrar or provider-wide
credentials on a public bulletin host.

## Constrained transports

Basic public clients may use plain HTTP for compatibility; private and administrative
operations require HTTPS. If offering HTTP compatibility, preserve GET/POST/PUT/MKCOL
methods and encoded request targets without redirects, challenges, caching or
prefetching of write routes. HEAD and OPTIONS must never publish. Test both normal
and unusual methods through the actual edge, not just against the local Go server.

DNS-only hosting exposes the origin directly; a DNS provider alone is not an HTTP
firewall. If using a proxy/CDN, verify its bot policies, URL normalization, caching,
request limits and streaming behavior against the protocol before enabling it.

## State and recovery

The database contains private rooms and operator state. Never publish it as a public
dataset. Use the application's consistent online backup operation or a separately
validated SQLite-aware replication system, with access-controlled off-host storage.
Encrypt independent recovery copies and keep recovery keys separate from the host.

Regularly restore into a separate isolated instance and check integrity, identity
continuity, private-room permissions and archive-cursor recovery. A successful backup
upload is not a successful restore test. Provider snapshots supplement, rather than
prove, application-consistent recovery. Publish only durability claims actually tested.

Private read grants require schema8: the private grant table and each private room's
durable access epoch are part of authorization state. Migration eagerly initializes
epochs, preserving older ordinary room JSON. Older binaries must refuse schema8;
never force a schema downgrade or perform a binary-only rollback across that boundary.
Validate restores against an explicitly selected compatible schema range, including
the grant table and room epoch column. After restoring an older backup, rotate the
service recovery generation offline before traffic. Test lost enrollments, restored
revocations and private reader denial on the isolated copy. Classification absent
from an old backup cannot be magically remembered by the restored service.

Monitor disk headroom, process health, backup lag, restore-drill age and publication
failures. Define safe rollback and incident-response procedures before promising an
availability SLA. Bound storage admission so growth cannot silently consume the host.

## Optional public datasets

Run the [publisher](DATASET.md) as a separate restricted user. It should read only the
eligible public export API, never the private database or administrator token. Start
with a dry run; review publication terms, source-specific rights and the dataset card
before enabling a scoped publishing credential and schedule. Do not mistake a public
archive for a private backup or assume downloaded copies can always be retracted.

## Acceptance checks

An optional [external-reference view](SOURCE_SYNC.md#optional-guarded-external-reference-publication)
requires a separate source writer, reviewed collection/public-reference permissions
and three protected publication files. Leave all `REFERENCE_*` settings unset until
that workflow is explicitly provisioned. Do not serve its policy files statically,
share its private SQLite catalog with the app, or replace unavailable snapshots with
cached responses. Test withdrawal, expiry and failed publication before scheduling it.
This workflow never feeds native events, identities, work or public datasets.

Check SSR without JavaScript, read/write compatibility, exact retry behavior, signed
identity portability, private-room exclusion across every public surface, live
moderation, attachment expiry, HTTP/TLS boundaries and public export eligibility.
Use isolated fixtures; avoid adding fake production users or undeclared seed traffic.
See the [README](../README.md#verify) and [security model](../SECURITY.md).
