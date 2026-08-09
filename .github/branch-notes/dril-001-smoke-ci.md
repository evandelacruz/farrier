# Branch: c/exciting-tesla-rgau46

Finishes **DRIL-001**: `drill` must restore the most recent backup to a
scratch target, boot the full stack, **run a smoke CI job**, and report
success or the specific failing step.

Everything except the smoke CI job already landed. `internal/core/drill`
resolves the newest snapshot at the destination, restores it onto the
scratch target, boots the full stack there, leaves DNS alone, and names the
failing step. Both `farrier drill` and `POST /drill` reach it. FORGE-005
(colocated Actions runner) and DRIL-002 (quarantine) have landed, so the
drilled instance has a runner and is already mute and unroutable.

Work:

1. **Point the drilled runner at the drilled instance.** The colocated
   runner connects to `https://<bundle domain>/`, and a drill leaves DNS
   pointing at production — so a drilled runner resolves the domain to the
   *production* instance and, holding the same bundle runner secret, polls
   production's job queue and would run production's CI on the scratch host.
   A quarantined deployment now gives Caddy the bundle domain as a Compose
   network alias, so on the drilled host the domain resolves to the drilled
   Caddy. No DNS record is touched, no port is published, and the runner's
   configuration is byte-identical to production's.
2. **Run the smoke job.** `forge.SmokeCI` creates a scratch repository on
   the drilled instance holding one trivial workflow, waits for the run the
   commit triggers, and reports success or what failed — all of it inside
   the forgejo container over the same `docker compose exec` path admin
   bootstrap and runner registration already use, so nothing new is exposed
   and the API token it mints never leaves that container.
3. **Fold it into the report.** One more step in the drill sequence, into
   `drill.Report` exactly like the restore steps.

Quarantine is tightened, never loosened: no port opened, no bypass flag, no
DNS change, no change to the runner's trust boundary. Snapshot verification,
the four-kind state model, and the identity model are untouched.

Teardown of the scratch target is DRIL-003 and is deliberately not here.

See `docs/functional-requirements.md` § DRIL and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR
merges.
