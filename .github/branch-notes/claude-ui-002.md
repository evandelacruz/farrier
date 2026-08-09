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
- `GET /snapshots?to=<uri>` in `internal/api` returns that list.
- `farrier snapshots -to <uri>` exposes it from the CLI, so backup history
  is reachable from both frontends.

Drill results need no new endpoint: a drill is a job, and its report reaches
both frontends through the CORE-002 event stream the CLI already renders.

Promotion honours FAIL-002 in the browser: the view shows the resolved
snapshot's age and requires a deliberate confirm before `POST /promote` is
sent with `confirm: true`.

This file exists for conductor tracking and can be deleted once the PR
merges.
