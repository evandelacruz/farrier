# Branch: claude/impt-002

Tracking branch for IMPT-001 and IMPT-002 — bringing repositories in from
GitHub or GitLab.

`import` doesn't exist in the tree yet, and per spec.md ("Importing
repositories") it wraps Forgejo's built-in migration API as one call:
code, full history, LFS objects, default branch, and an optional mirror
flag are all fields of that same request. IMPT-002 ("optional continuous
mirror sync") cannot be built, reviewed, or run without that base call
existing, so this PR lands the decided-but-unbuilt prerequisite (IMPT-001)
alongside the mirror option (IMPT-002) it was assigned. IMPT-003
(per-repository batch reporting and rollback) is out of scope here and
stays open for a later pass.

New `internal/core/importer` package calls Forgejo's `POST
/api/v1/repos/migrate` against the target instance, with `cmd/farrier
import` as the thin CLI skin.

See `docs/functional-requirements.md` § IMPT and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
