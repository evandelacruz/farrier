# Branch: c/hopeful-ptolemy-0ya4u4

Implements **BKUP-006**: `backup` must exist as an operator-invokable
command that runs BKUP-001 through BKUP-005 end to end against a
destination the operator names, reporting progress as one CORE-002 job,
and must be reachable from both the CLI and the API.

Adds `backup.Backup` (`internal/core/backup/orchestrate.go`), the first
real caller of `OpenDestination`, `Run`, `Encrypt`, `Verify`, and `Write`:
it resolves the destination, captures a snapshot, age-encrypts it,
re-verifies the decrypted form of the archive before it leaves the host,
and writes it — all as one job. `cmd/farrier backup` and `POST /backup`
both call it. Also adds `blob.New` (a generic blob-driver constructor, the
same pattern `keystore.New` already has) and exports
`forge.DatabasePath`, both small prerequisites the orchestrator needed
that weren't built yet.

See `docs/functional-requirements.md` § BKUP and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
