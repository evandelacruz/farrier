# Branch: claude/fail-003

Investigates **FAIL-003**: queued CI jobs must re-dispatch to runners after
promotion without operator action.

FAIL-001 (`internal/core/promote`, merged in #65) already sets
`restore.Options.ReconcileCI`, which runs `forge.ReconcileCI` (FORGE-004)
against the snapshot's database before it ships to the standby host and
before services start — resetting every orphaned `running` run/job to
`queued` so Forgejo's own scheduler dispatches them once the forgejo
container starts, with no operator action. That is the whole of what
Farrier owns for FAIL-003: the DB-state half of "re-dispatch to runners."
Runners actually reconnecting to receive that dispatch is FAIL-005's
territory, not absorbed here.

Existing coverage (`internal/core/forge/reconcile_test.go`,
`internal/core/restore/ci_test.go`, `internal/core/promote/promote_test.go`)
already proves the reset happens, happens before placement, and leaves
non-running jobs untouched at the unit level. This branch checks whether
that adds up to the full requirement or leaves a real gap, and adds the
test(s) needed to close it either way.

See `docs/functional-requirements.md` § FAIL and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
