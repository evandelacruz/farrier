# Branch: claude/bkup-003

Tracking branch for BKUP-003 — every snapshot must be age-encrypted before
leaving the host. Adds `internal/core/backup.Encrypt`: tars the plain
snapshot directory `backup.Run` (BKUP-001) writes and age-encrypts it to a
single archive for a given `age.Recipient`, emitting its own CORE-002 step
so it composes into the same job a future `backup` orchestrator drives
alongside `Run`.

See `docs/functional-requirements.md` § BKUP and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
