# Branch: c/hopeful-ptolemy-wyagt3

Implements **FAIL-001**: `promote` must execute failover as one command —
restore the latest snapshot onto a target host, verify, start services,
reconcile CI, update DNS.

Adds a new `internal/core/promote` package that sequences already-landed
pieces rather than reimplementing them: `restore.Restore` (RSTR-001..003,
which already ends with `deploy.Up` starting services) for restore-verify-
start, `forge.ReconcileCI` (FORGE-004) for CI reconciliation, and
`internal/core/dns`'s driver interface (DNS-003's print-if-unconfigured
rule) for the DNS flip. `restore.Options` gains a `ReconcileCI` flag so the
reset runs against the snapshot's database file locally, before it is
shipped to the host — the only way `forge.ReconcileCI`'s direct SQLite
update can run, and consistent with tech-spec.md's existing "executed
before services start" note.

`cmd/farrier promote` and `POST /promote` are the thin CLI/API skins.

See `docs/functional-requirements.md` § FAIL and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
