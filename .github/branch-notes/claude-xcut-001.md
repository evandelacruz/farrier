# Branch: claude/xcut-001

Tracking branch for XCUT-001 (remaining slice) — extend the relocated-bundle
portability proof to the `backup` CLI/core path.

`TestUpSucceedsFromBundleCopiedToAnotherDirectory` already proves `up`
survives a bundle physically relocated after the directory it was saved to
no longer exists. This adds `TestBackupSucceedsFromBundleCopiedToAnotherDirectory`
(`internal/core/backup`), the same proof for `backup`: the keystore driver,
blob adapter, and age backup key it resolves from a loaded bundle all work
after relocation.

XCUT-001 stays `partial` — restore, promote, upgrade, and drill still need
the same proof once each lands.

See `docs/functional-requirements.md` § XCUT and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
