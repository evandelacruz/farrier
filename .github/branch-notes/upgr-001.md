# Branch: claude/upgr-001

Implements **UPGR-001**: `upgrade` must run only against a healthy
instance and must execute: backup, bump pinned version, apply migrations,
verify.

`docs/spec.md` "Version pinning" already settles the sequence and its one
load-bearing ordering constraint: the pre-upgrade backup must be taken
*before* the pinned version is bumped, so the snapshot it produces still
records the pre-upgrade Forgejo version and restores cleanly without
migrating (UPGR-003: "Schema migrations run during upgrades, never during
restores").

New package `internal/core/upgrade` sequences already-landed pieces, as one
CORE-002 job, rather than reimplementing any of them:

1. **Health gate** — `status.Check` (STAT-001), refusing with the specific
   reason (down service, invalid TLS, exhausted disk) the way `restore`
   and `backup` already refuse and name defects.
2. **Pre-upgrade backup** — `backup.BuildOptions` + `backup.Backup`
   (BKUP-006), run against the target bundle *before* it is bumped, so
   `ForgejoVersion` still records the pre-upgrade pin. This backup is never
   touched again on any exit path, so a failed upgrade leaves the operator
   with a pre-upgrade snapshot and a working path back (UPGR-002, a
   separate open ID this PR doesn't otherwise implement).
3. **Bump the pinned version** — the operator-named Forgejo image resolves
   to a digest via `registry.Resolver` and overrides a copy of the
   bundle's manifest, Compose re-rendered from that override
   (`orchestrate.Render`), the same pattern `restore.pinnedBundle` already
   uses. Unlike restore's in-memory-only override, the bump is saved back
   to the bundle directory (`bundle.Bundle.Save`) so the pin survives past
   this run.
4. **Apply migrations** — `deploy.Up` converges the host to the bumped
   bundle; Forgejo migrates its own schema when its container starts on
   the newer image. No new migration logic is written — that's Forgejo's
   job, this PR only sequences the restart safely.
5. **Verify** — `status.Check` again, against the bumped bundle, through
   the same health check the gate uses.

Reachable from `cmd/farrier upgrade` and `POST /upgrade` (API-001), both
thin over the core, wired the same way `backup`'s CLI/API skins resolve
the target host, keystore, blob adapter, and age backup key.

Tests: `internal/core/upgrade` covers the full sequence end to end
(including a direct proof that the pre-upgrade backup's manifest pins the
pre-upgrade version, not the post-upgrade one — UPGR-003's load-bearing
ordering), the health gate's refusal paths, a resolver failure leaving the
bundle unbumped, `Options.validate`, and the bundle-relocation portability
proof the other four core operations already carry (XCUT-001). `internal/api`
covers `POST /upgrade`'s request validation and wiring.

See `docs/functional-requirements.md` § UPGR and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR
merges.
