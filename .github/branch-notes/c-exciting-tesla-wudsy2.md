# Branch: c/exciting-tesla-wudsy2

Tracking branch for a fix, not a new requirement: `up` chowns the host
state directories to the uid Forgejo runs as and fails when that chown is
refused. An ordinary user cannot give files away to another uid, so `up`
fails as any non-root operator — which blocks the local-first path
UP-006 and ORCH-003 exist for.

The chown becomes best-effort, and `up` instead verifies the property that
actually matters: the forge, running as its own uid against the real
bind-mounted paths, can read and write its state. Same treatment for the
SSH host key directory.

No requirement ID; `docs/status.json` untouched.

This file exists for conductor tracking and can be deleted once the PR merges.
