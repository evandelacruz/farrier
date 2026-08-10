# Branch: c/exciting-tesla-qp3cob

Tracking branch for a defect fix in `internal/core/orchestrate` (ORCH-001,
ORCH-002, ORCH-003): a non-interactive SSH session cannot find `docker` on a
stock macOS Docker Desktop install, because such a session reads neither
`.zshrc` nor `.bash_profile`. `farrier up` against `ssh://user@localhost`
fails with `command not found: docker`.

No requirement changes state — `docs/status.json` is untouched.

This file exists for conductor tracking and can be deleted once the PR merges.
