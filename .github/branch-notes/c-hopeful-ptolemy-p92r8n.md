# Branch: c/hopeful-ptolemy-p92r8n

Tracking branch for the next slice of **API-001** — wiring `POST /import`
and `GET /status` onto `internal/api`, alongside the already-landed
`POST /init` and `POST /up`.

`IMPT-001..003` are fully landed, so `import` wires the same way `up`
does: parse the request, start a CORE-002 job, and call straight into
`importer.Run` (single repository) or `importer.RunBatch` (a `repos`
list) — the same functions `cmd/farrier import` calls. `status` is a
read, not a job: it dials the target, builds the bundle's keystore
driver, and calls `status.Check` synchronously, the same as `cmd/farrier
status`.

`backup`, `restore`, `promote`, `upgrade`, and `drill` stay unwired here:
`RSTR-*`, `FAIL-*`, `UPGR-*`, and `DRIL-*` are all still `open`, and
`backup` itself is incomplete without `BKUP-004` (verify) and `BKUP-005`
(write to destination) — both still open — so there is no complete core
operation yet for any of the five to wire onto (`docs/spec.md` "Backups":
`backup` must produce a "complete, verified, encrypted snapshot to an S3
URI or directory").

See `docs/functional-requirements.md` § API and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
