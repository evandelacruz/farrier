# Branch: claude/xcut-001-2

Tracking branch for XCUT-001 (remaining slice) — extend the relocated-bundle
portability proof to the `promote` core path.

`TestUpSucceedsFromBundleCopiedToAnotherDirectory`,
`TestBackupSucceedsFromBundleCopiedToAnotherDirectory`, and
`TestRestoreSucceedsFromBundleCopiedToAnotherDirectory` already prove `up`,
`backup`, and `restore` survive a bundle physically relocated after the
directory it was saved to no longer exists. This adds
`TestPromoteSucceedsFromBundleCopiedToAnotherDirectory`
(`internal/core/promote`), the same proof for `promote` (FAIL-001, now
landed): the keystore driver, blob adapter, and age identity it resolves
from a loaded bundle all work after relocation, through the full failover
sequence (restore.Restore + the DNS flip).

XCUT-001 stays `partial` — upgrade and drill still need the same proof once
each lands (UPGR-*, DRIL-* are still open).

Note: `claude/xcut-001` was already used and merged (PR #63); this slice
uses `claude/xcut-001-2` per the branch-naming rule for a reused ID.

See `docs/functional-requirements.md` § XCUT and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
