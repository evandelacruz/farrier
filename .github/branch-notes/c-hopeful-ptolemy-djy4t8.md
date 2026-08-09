# Branch: c/hopeful-ptolemy-djy4t8

Tracking branch for RSTR-003 — `restore` must refuse to proceed, naming the
specific missing or torn state, when manifest completeness, checksums, or
cross-consistency checks fail.

`internal/core/restore.Restore` already calls `backup.Verify` (BKUP-004)
before installing anything, and `backup.Verify` already implements all
three checks with named `Defect`s (`internal/core/backup/verify.go`). This
pass audits that path end to end — CLI and API surfaces both propagate the
named defects, nothing is installed before verification runs — and adds
test coverage proving restore refuses for each of the three failure
classes independently, with nothing installed when it does.

See `docs/functional-requirements.md` § RSTR-003, `docs/spec.md` "What the
system owns: verified restores", and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR
merges.
