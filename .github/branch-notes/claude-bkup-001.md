# Branch: claude/bkup-001

Implements **BKUP-001**: `backup` must produce a snapshot containing all
four state kinds plus a manifest with per-component checksums.

Adds `internal/core/backup`: `Run` captures database (`state.DatabaseExporter`),
git repositories (`state.GitExporter` + a new `backup.GitCapturer` that
streams each bare repo as a tar archive), blobs (`state.BlobExporter`), and
key material (`state.KeyExporter`) into a plain snapshot directory, checksums
every captured file, and writes `snapshot-manifest.json` (Forgejo version,
timestamp, checksum algorithm, one entry per captured file). Progress is
emitted through a CORE-002 job event stream.

Also fixes a drift bug between STATE-004 and INIT-003 found while wiring
this up: `state.KeyExporter` enumerated `tls_issuer_certificate`, which
`init` never stores, and omitted `ssh_host_key.pub`, which `init` does store
— so a real backup against a real bundle would fail immediately on resolve.
`state.KeyExporter`'s key set now matches what `init` actually generates.

Out of scope here (separate IDs): the push-hold window around git capture
(BKUP-002), snapshot encryption (BKUP-003), verification at creation
(BKUP-004), and writing to an S3/filesystem destination (BKUP-005). This PR
produces the plain, unencrypted snapshot those build on.

See `docs/functional-requirements.md` § BKUP and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
