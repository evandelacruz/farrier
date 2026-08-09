# Branch: c/exciting-tesla-to3hxw

Implements **DRIL-001**: `drill` restores the most recent backup to a
scratch target, boots the full stack, and reports success or the specific
failing step.

Nothing in the sequence is new machinery. `restore.Restore` (RSTR-001..004)
already fetches the newest snapshot from a destination when no key is
named, verifies it, refuses on a failed verification naming the defect,
installs the original identity, and ends by running `deploy.Up`
(UP-001..004) — which is what "boot the full stack" means here. `drill` is
the sequencing package over it, in the shape `promote` (FAIL-001) and
`upgrade` (UPGR-001) already established:

1. **Latest snapshot, resolved once.** `backup.LatestSnapshotKey` against
   the destination, resolved by drill rather than left implicit inside
   restore, so the report and the events can name which snapshot was
   drilled.
2. **Restore onto the scratch target, and nothing else.** Drill never
   touches DNS — flipping the domain is `promote`'s job, and a drill that
   repointed it would take production down.
3. **Report the specific failing step.** Drill's distinguishing value.
   It watches the CORE-002 step stream it relays and, on failure, names
   the step that failed rather than reporting "the drill failed".
4. **Thin skins.** `cmd/farrier drill` and `POST /drill` — the latter
   closes API-001's remaining line.
5. **XCUT-001.** A relocated-bundle proof for drill, closing XCUT-001's
   remaining line.

Two things are raised on the PR rather than decided here: the smoke CI job
needs a CI runner the bundle does not ship, and booting a restored
instance that carries production's identity before DRIL-002's quarantine
lands is a real hazard. Both are Evan's call.

See `docs/functional-requirements.md` § DRIL and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR
merges.
