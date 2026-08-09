# Branch: claude/fail-002

Implements **FAIL-002**: `promote` must display the age of the snapshot
being promoted and require operator confirmation before acting.

Extends `internal/core/promote` (FAIL-001, PR #65) with a confirmation
gate in front of the already-landed promote sequence — not a new command.

- `backup.SnapshotAge` resolves which snapshot key `promote` would restore
  (the given key, or the newest object in the destination — the same
  resolution `backup.LatestSnapshotKey` and `status.ReplicationLag`
  already use) and its age as of now.
- `cmd/farrier promote` prints the resolved snapshot key and its age, then
  requires the operator to type `yes` at an interactive prompt before
  promote.Promote runs. A `-yes` flag lets a scripted caller supply that
  consent up front instead, still displaying the age either way.
- `POST /promote` cannot prompt on a terminal, so it takes an explicit
  `confirm: true` field. Missing or `false` returns 400 with the resolved
  snapshot key and age in the error message and starts no job — refusal is
  the default, not silent promotion.

See `docs/functional-requirements.md` § FAIL and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
