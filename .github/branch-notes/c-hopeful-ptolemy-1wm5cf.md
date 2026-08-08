# Branch: c/hopeful-ptolemy-1wm5cf

Tracking branch for XCUT-002 — the CLI must render the CORE-002 event
stream in the terminal for every long-running operation.

`init`, `up`, and `import` already subscribed to their job's event stream
and printed it, but each command duplicated its own subscribe/print/wait
boilerplate and there was no test coverage of the rendering path itself.
This PR extracts that into one shared, tested `runJob`/`printEvents` in
`cmd/farrier/events.go` and switches all three commands to use it, so the
CORE-002 rendering path is written and verified once instead of by copy.

CLI-only: no core or API surface changes.

See `docs/functional-requirements.md` § XCUT and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
