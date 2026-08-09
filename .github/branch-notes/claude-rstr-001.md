# Branch: claude/rstr-001

Tracking branch for RSTR-001 — `restore` must rebuild a complete working
instance on a fresh host from a snapshot and a bundle.

Previously halted on an architecture question (where a bundle's git
repositories live on the host filesystem). That question is answered —
UP-004 landed (PR #58) and pins forge state to `<RemoteDir>/state/git` and
`<RemoteDir>/state/gitea`, bind-mounted into the forgejo container. This
pass implements RSTR-001 on top of it: `internal/core/restore.Restore`
fetches, decrypts, and verifies a snapshot (reusing `backup.Verify`),
installs its key material into the target keystore, places its git data and
database directly onto the UP-004 host paths, restores blobs through the
target's `blob.Adapter`, and runs `deploy.Up` against the state just placed.

See `docs/functional-requirements.md` § RSTR, `docs/tech-spec.md` §
"Restoring an instance (`restore`, RSTR-001)", and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
