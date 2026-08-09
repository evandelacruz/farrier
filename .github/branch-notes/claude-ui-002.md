# Branch: claude/ui-002

Implements **UI-002**: the dashboard must cover status, replication lag,
backup history, drill results, and promotion, each backed by the same core
operations as the CLI.

Builds on UI-001's plain static assets — no framework, no build toolchain,
`go:embed` only — and keeps the dashboard's zero-logic posture: every view
is an `internal/api` call over the same loopback origin the page is served
from.

New core + API surface for the one view nothing exposed yet:

- `internal/core/backup.History` lists a destination's snapshots with key,
  size, capture time, and age, reusing the same "list the destination, read
  Modified" measurement `LatestSnapshotKey` and `SnapshotAge` already use.
- `GET /snapshots?to=<uri>` in `internal/api` returns that list. It is the
  one verb with no CLI counterpart, because spec.md "Interfaces" assigns
  backup history to the dashboard and spec.md's CLI table is ten commands.

Status, replication lag, drill, and promotion needed no new endpoint:
`GET /status` already carries the first two, and a drill or a promotion is
a job whose report reaches both frontends on the CORE-002 event stream.

Promotion honours FAIL-002 in the browser: the view shows the resolved
snapshot's age and requires a deliberate confirm before `POST /promote` is
sent with `confirm: true`.

This file exists for conductor tracking and can be deleted once the PR
merges.
