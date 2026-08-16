# Branch: runner-labels

Tracking branch for a defect fix in `internal/core/forge` and
`internal/core/deploy` (FORGE-005): `up` deploys and registers the colocated
runner, every step reports green, and the runner answers to no label at all.
The admin runner list shows it with an empty label set, and a pushed
workflow queues forever with no error anywhere.

Labels are runner-side configuration the daemon declares when it connects —
Forgejo has no labels of its own to hand it. `up` now ships a `config.yaml`
next to the runner secret in the mounted data directory, declaring `docker`
and `ubuntu-latest` against one job-container image, and points both
`create-runner-file` and the daemon at it. Registration also seeds the same
label set on the admin row so the runner reads correctly before its first
connect.

This is a defect fix against FORGE-005 as written — a runner answering to no
label does not make "a workflow pushed to a fresh deployment run." No
requirement amendment, no `docs/status.json` change.

Stacks on PR #132 (`c/exciting-tesla-ans5b9`), which touches the same two
files.

This file exists for conductor tracking and can be deleted once the PR
merges.
