# Branch: claude/impt-003

Tracking branch for IMPT-003 — `import` must report per-repository
success or failure and must leave no partially-registered repository
behind on failure.

`internal/core/importer` currently migrates exactly one repository per
`Run` call (IMPT-001, IMPT-002). This PR adds:

- A batch entry point (`importer.RunBatch`) that migrates many
  repositories against one target instance in a single job, reporting
  each repository's own success/failure rather than one pass/fail for
  the whole run.
- Best-effort cleanup on a single repository's migration failure: a
  `DELETE` against the target's repo API so a failed migration never
  leaves a partially-registered repository behind, whether Forgejo
  rejected the migration outright or the request failed after the
  repository record was created.
- `cmd/farrier import -file <manifest.yaml>` as the thin CLI skin for
  batch imports (repository list only — target/source credentials stay
  in the environment, never in the file).

See `docs/functional-requirements.md` § IMPT and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR
merges.
