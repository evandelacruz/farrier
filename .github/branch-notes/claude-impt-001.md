# Branch: claude/impt-001

Tracking branch for IMPT-001 — `import` must migrate repositories from
GitHub or GitLab given a source URL and token: code, full history, LFS
objects, default branch. `internal/core/importrepo.Run` wraps Forgejo's own
`POST /api/v1/repos/migrate` API rather than reimplementing git/LFS
transfer; `cmd/farrier import` is the CLI skin.

See `docs/functional-requirements.md` § IMPT and `docs/status.json`.
