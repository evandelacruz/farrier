# Branch: claude/bkup-005

Tracking branch for BKUP-005 — `backup` must write to an S3-compatible
URI or a filesystem path.

`internal/core/backup.Run` (BKUP-001/002) captures a plain snapshot
directory and `backup.Encrypt` (BKUP-003) turns it into the single
age-encrypted archive that leaves the host. Nothing yet puts that archive
anywhere durable. This PR adds:

- `backup.OpenDestination(uri)` — resolves the golden path's
  `backup --to <uri>` (spec.md "Golden path") into a `blob.Adapter`: an
  `s3://` URI selects the `s3` adapter (BLOB-002, with credentials read
  from `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`, never the URI or a
  flag), anything else is a filesystem directory path and selects the
  `local` adapter (BLOB-001).
- `backup.Write(ctx, job, dest, archivePath, timestamp)` — streams the
  encrypted archive to `dest` under `SnapshotKey(timestamp)`, the
  snapshot naming convention tech-spec.md's "Status" section was waiting
  on (STAT-001's last-backup age, STAT-002's replication lag both list a
  destination and take the newest `Modified` time).

Core-only: no CLI or API surface yet. The `backup` command itself — one
job composing `Run`, `Encrypt`, `Write`, and BKUP-004's still-unimplemented
verification — is later work, same as BKUP-001..003 before it.

See `docs/functional-requirements.md` § BKUP and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR
merges.
