# Branch: claude/upgr-002

Implements **UPGR-002**: a failed `upgrade` must leave the operator with
the pre-upgrade backup and a working path back to the pre-upgrade version.

UPGR-001 already landed the sequence and its load-bearing ordering: the
pre-upgrade backup is taken *before* the pinned version is bumped, so the
snapshot still records the pre-upgrade Forgejo version, and RSTR-002
guarantees `restore` runs that exact recorded version. Restoring that
snapshot therefore *is* the path back. UPGR-002 is about a failed upgrade
neither destroying nor obscuring it.

Work in `internal/core/upgrade`:

1. **Never lose the snapshot on a failure path.** Audit every early return,
   error branch, and deferred cleanup so nothing removes the pre-upgrade
   snapshot from the destination.
2. **Name where it is.** The destination, the snapshot key, and the
   pre-upgrade Forgejo version go into the failure and into the CORE-002
   event stream, so the dashboard shows them as well as the terminal.
3. **Name what to run.** The operator gets the exact `restore` invocation
   that puts the pre-upgrade version back, not a hint they must derive.
4. **Prove it.** Tests fail an upgrade at each post-backup step and assert
   the snapshot still exists at the destination, still verifies, and still
   pins the pre-upgrade version.

Nothing about snapshot verification, the four-kind state model, the
identity model, or migration timing (UPGR-003) changes.

See `docs/functional-requirements.md` § UPGR and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR
merges.
