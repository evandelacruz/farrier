# Branch: c/hopeful-ptolemy-yg76ja

Tracking branch for **XCUT-001** — every operation must work from any
machine holding the bundle and key access; nothing may depend on the
machine that ran `init`.

Found and closed a real gap while auditing the landed operations that
consume a bundle (`init`, `up`, `status`): the keystore `file` driver and
the `local` blob adapter took `config.path` verbatim, including a relative
one. A relative path gets baked into the manifest at `init` time and then
silently re-resolves against whatever directory a *later* command happens
to run from — a different one on another machine, or even the same
machine from a different shell session — instead of failing loudly or
staying put. `keystore.New` and `blob.NewLocal` now reject a relative
`config.path`/`root` at construction.

`TestUpSucceedsFromBundleCopiedToAnotherDirectory`
(`internal/core/deploy/portability_test.go`) proves the fix and the
broader XCUT-001 claim together: a bundle is saved, physically copied to a
different directory tree, the original deleted, and `deploy.Up` still
succeeds against the relocated bundle — one layer past CORE-001's
narrower manifest-round-trip proof, since this actually drives keystore
resolution and the full `up` sequencing.

Recorded `partial` in `docs/status.json`: this covers every currently-landed
operation that touches a bundle (`import` never does). The same
relocated-bundle proof extends to backup, restore, promote, upgrade, and
drill as each lands (BKUP-\*, RSTR-\*, FAIL-\*, UPGR-\*, DRIL-\* are still
open) — none exist yet to test.

See `docs/functional-requirements.md` § XCUT and `docs/status.json`.
