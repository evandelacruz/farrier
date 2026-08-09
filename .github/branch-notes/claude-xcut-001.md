# Branch: claude/xcut-001

Tracking branch for XCUT-001 (remaining slice) — extend the relocated-bundle
portability proof to the `restore` core path.

`TestUpSucceedsFromBundleCopiedToAnotherDirectory` and
`TestBackupSucceedsFromBundleCopiedToAnotherDirectory` already prove `up`
and `backup` survive a bundle physically relocated after the directory it
was saved to no longer exists. This adds
`TestRestoreSucceedsFromBundleCopiedToAnotherDirectory`
(`internal/core/restore`), the same proof for `restore`: the keystore
driver, blob adapter, and age backup key it resolves from a loaded bundle
all work after relocation.

XCUT-001 stays `partial` — promote, upgrade, and drill still need the same
proof once each lands (FAIL-*, UPGR-*, DRIL-* are still open).

See `docs/functional-requirements.md` § XCUT and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
