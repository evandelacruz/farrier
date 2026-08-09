# Branch: claude/rstr-002

Tracking branch for RSTR-002 — `restore` must run the exact Forgejo version
recorded in the snapshot.

Builds on RSTR-001 (`internal/core/restore`, PR #56): `restore.Restore`
already fetches, decrypts, and verifies a snapshot and runs `deploy.Up`
against the target bundle's currently pinned image. RSTR-002 overrides that
image with the one the snapshot manifest recorded (`backup.Manifest.
ForgejoVersion`), re-rendering Compose from the override before `deploy.Up`
ever runs, so restore always boots the exact version the snapshot was
captured from (spec.md "Version pinning") rather than whatever the target
bundle's own farrier.yaml currently pins.

See `docs/functional-requirements.md` § RSTR-002, `docs/tech-spec.md` §
"Restoring an instance (`restore`, RSTR-001)", and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
