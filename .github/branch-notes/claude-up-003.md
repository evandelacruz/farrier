# Branch: claude/up-003

Finishes **UP-003**: re-running `up` against a live host must be safe and
must converge that host to the bundle definition.

Closes the TLS re-run gap: `deploy.configureTLS` now resolves the
certificate `init` already persisted as bundle key material (INIT-003) and
passes it to the renewal-aware `acme.EnsureValid` (ACME-002) instead of
issuing a fresh certificate from a new ACME account on every call. An
ordinary re-run reuses the persisted certificate untouched; the rare
renewal-due branch issues a fresh certificate for that deploy and reports
through the job's event stream that it was not persisted (that write-side
gap belongs to ACME-002, tracked separately) — it does not fail the
deploy or touch the keystore's no-overwrite invariant.

See `docs/functional-requirements.md` § UP and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
