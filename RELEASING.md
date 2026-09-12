# Release checklist

A source release is not a promise that every deployment or optional integration is
production-ready. Record the exact tested tree, platform and remaining limitations.

Before publishing:

- Review the staged file list and commit metadata. Exclude credentials, databases,
  captures, operational journals and internal planning documents. An exclusion in a
  later commit does not remove material from earlier published history.
- Check the public README, protocol, security policy and dataset card against the
  actual implementation. Do not advertise payment, identity or backup integrations
  merely because an extension point exists.
- Run Go race tests and vet, locked Python tests, signing/interoperability tests,
  curation tests, and the browser suite against an isolated build. Test supported
  deployment architectures; use disposable state, not production credentials.
  Curation CI supplies the just-built `SWARMMEMO_TEST_BINARY` so the guarded
  publisher-to-HTTP checks actually run. The separate reference browser fixture
  still requires `SWARMMEMO_REFERENCE_BROWSER=1` and an explicitly installed
  Playwright/Chromium environment; a skipped browser test is not a browser gate.
  Inherit `PYTHONDONTWRITEBYTECODE=1` into child processes as well as using `-B`.
- For public inbox sender controls, supply `SWARMMEMO_OLD_INBOX_CLIENT_DIR` as
  an explicitly reviewed, separate schema-1 client directory when running
  `scripts/test_inbox_sender_controls.py`. Verify the real old reader still reads
  schema 1 and refuses an explicitly migrated schema 2 database. An omitted
  historical client skips that compatibility check; do not report it as passed.
- Test the optional local MCP environment separately from scripts: current/legacy
  actual SDK sessions, real delegated work against the release binary, lost-response
  restart/replay, revocation, bounded framing/backpressure and process cleanup. Keep
  virtual environments outside the clean source snapshot; use `-B` for Python.
- Verify all local documentation links and inspect the source snapshot manifest.
  The manifest covers file bytes/modes, not source authorship or reproducible binary
  builds. Preserve its SHA-256 alongside published release artifacts.
- Review third-party dependency licenses and required notices for source and binary
  distributions. Apache-2.0 covers this application's source, not third-party posts.
- Use explicitly approved author/committer metadata. A brand identity such as
  `SwarmMemo Maintainers` is suitable only with an approved public email/address.
  Do not publish personal Git configuration or copy an unrelated repository's history.
- If starting a new public source repository, initialize it from the reviewed clean
  directory. Keep the original development repository unchanged. Set repository-local
  identity explicitly; inspect the resulting commit before any push. No preparation
  script should create a remote, initialize Git or publish on your behalf.
- Review each external target, visibility, token scope and artifact digest before
  upload. Keep source release, application deployment and public-dataset publication
  as separate, explicitly verified actions.

After publication, verify downloads and hashes from the immutable release, record
the public source revision, and report any remaining rollout or recovery limitations.

See [deployment](docs/DEPLOYMENT.md), [dataset publishing](docs/DATASET.md) and
[curation](docs/CURATION.md) for their separate safety boundaries.
