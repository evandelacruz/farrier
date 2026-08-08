# Functional Requirements

What the system must observably do, stated testably from the operator's seat. Rationale and design decisions live in [spec.md](spec.md); implementation structure lives in [tech-spec.md](tech-spec.md).

Every requirement has a stable ID. IDs are the unit of work: agents implement them, commits and PRs cite them, and [status.json](status.json) records what has shipped. **Never renumber an ID.** Requirement language: **must** marks behavior the system is incomplete without.

## Dependency order

Foundation first (CORE, KEY, ORCH), then the state layer, then the commands built on it. The gates are encoded in `tools/fleet/src/backlog.ts`:

- CORE-001–003 precede everything.
- KEY, BLOB, and STATE precede BKUP.
- BKUP precedes RSTR; RSTR precedes FAIL, UPGR, and DRIL.
- ORCH and FORGE precede UP.
- DNS-001 precedes FAIL-004; ACME-001 precedes INIT-002.
- API-001 precedes all UI.

---

## CORE — engine foundation

- **CORE-001** · The bundle must be a plain directory containing a YAML manifest (domain, image digests, driver config, state declarations) and rendered Compose files, holding zero key material. It must function identically after being copied to another machine, given key access.
- **CORE-002** · Every long-running operation must be a job identified by ID, emitting a single progress event stream (`jobId`, `step`, `state`, `detail`, `timestamp`) that both frontends render.
- **CORE-003** · Driver extension points must be published as a Go interface plus an exec protocol (JSON on stdin/stdout), so third parties ship drivers as standalone executables without Go.

## KEY — keystore drivers

- **KEY-001** · The `file` driver must resolve key material from a local path.
- **KEY-002** · The `command` driver must resolve key material from the stdout of any operator-specified command.
- **KEY-003** · Key material must never appear in logs, event streams, command output, or the bundle directory.

## BLOB — blob adapters

- **BLOB-001** · The `local` adapter must provide list, get, and put against a filesystem path, streaming.
- **BLOB-002** · The `s3` adapter must provide the same operations against any S3-compatible endpoint.

## ORCH — host orchestration

- **ORCH-001** · The system must reach hosts over SSH using the operator's existing agent or key file, requiring nothing on the host beyond Docker and SSH.
- **ORCH-002** · The system must render Compose definitions from the manifest, ship them to the host, and converge the host to that definition idempotently; drift is overwritten.

## FORGE — forge configuration

- **FORGE-001** · Forgejo `app.ini` must be rendered fully from the manifest, so the install wizard is pre-answered and never presented.
- **FORGE-002** · The first admin account must be created during deployment, with credentials emitted exactly once through the event stream.
- **FORGE-003** · Fork pull requests on public repositories must require maintainer approval before CI runs, by default.
- **FORGE-004** · CI reconciliation must reset `running` jobs to `queued` so they re-dispatch without operator action.

## ACME — certificates

- **ACME-001** · The system must issue TLS certificates via ACME DNS-01, in-process, and must fail with the reason when the challenge fails.
- **ACME-002** · Certificates must renew automatically before expiry, and `status` must warn when a certificate is within 14 days of expiring.

## DNS — DNS drivers

- **DNS-001** · The `cloudflare` driver must set and delete records via API.
- **DNS-002** · The `rfc2136` driver must do the same via nsupdate.
- **DNS-003** · With no driver configured, every DNS-changing operation must print the exact records to change instead of failing.
- **DNS-004** · All bundle records must be created with a 60-second TTL.

## STATE — state export

- **STATE-001** · Git data must be exportable as a mirrorable set of remotes.
- **STATE-002** · The database must be exportable as a consistent snapshot via SQLite's online-backup API, with no service interruption.
- **STATE-003** · Blobs must be exportable through the blob adapter interface.
- **STATE-004** · Key material must be enumerable as a state kind for capture and installation.

## INIT — bundle creation

- **INIT-001** · `init` must create a bundle from a DNS name and a keystore target.
- **INIT-002** · `init` must prove control of the DNS zone via an ACME DNS-01 challenge and fail, with the reason, when proof fails.
- **INIT-003** · `init` must generate all key material: Forgejo `SECRET_KEY` and `INTERNAL_TOKEN`, LFS JWT secret, TLS certificates, SSH host keys, and the age backup key.

## UP — deployment

- **UP-001** · `up` must deploy the full stateless layer given only `ssh://user@host` and a bundle.
- **UP-002** · `up` must complete with the forge serving HTTPS at the bundle domain and usable in a browser immediately.
- **UP-003** · Re-running `up` against a live host must be safe and must converge that host to the bundle definition.

## IMPT — repository import

- **IMPT-001** · `import` must migrate repositories from GitHub or GitLab given a source URL and token: code, full history, LFS objects, default branch.
- **IMPT-002** · `import` must support optional continuous mirror sync from the source.
- **IMPT-003** · `import` must report per-repository success or failure and must leave no partially-registered repository behind on failure.

## BKUP — backup

- **BKUP-001** · `backup` must produce a snapshot containing all four state kinds plus a manifest with per-component checksums.
- **BKUP-002** · `backup` must run against a live instance: reads and fetches stay available throughout; a push during the hold is rejected outright (never queued or buffered) with an explicit message; the hold covers only the database backup and ref recording, not the (much larger) object tar, so its duration does not grow with the amount of git data held; and the hold must release on every exit path — success, error, panic, or a canceled context — bounded by a configurable ceiling so a wedged capture cannot hang pushes indefinitely.
- **BKUP-003** · Every snapshot must be age-encrypted before leaving the host.
- **BKUP-004** · `backup` must verify the snapshot at creation and exit nonzero, naming the specific defect, when verification fails.
- **BKUP-005** · `backup` must write to an S3-compatible URI or a filesystem path.

## RSTR — restore

- **RSTR-001** · `restore` must rebuild a complete working instance on a fresh host from a snapshot and a bundle.
- **RSTR-002** · `restore` must run the exact Forgejo version recorded in the snapshot.
- **RSTR-003** · `restore` must refuse to proceed — naming the specific missing or torn state — when manifest completeness, checksums, or cross-consistency checks fail.
- **RSTR-004** · A restored instance must present the original SSH host keys and TLS identity, so existing clones, remotes, and known-hosts entries work unchanged.

## FAIL — failover

- **FAIL-001** · `promote` must execute failover as one command: restore the latest snapshot onto a target host, verify, start services, reconcile CI, update DNS.
- **FAIL-002** · `promote` must display the age of the snapshot being promoted and require operator confirmation before acting.
- **FAIL-003** · Queued CI jobs must re-dispatch to runners after promotion without operator action.
- **FAIL-004** · `promote` must apply the DNS record change through a configured driver, or print the exact change.
- **FAIL-005** · Remote runners must reconnect after promotion with no re-registration.

## UPGR — upgrade

- **UPGR-001** · `upgrade` must run only against a healthy instance and must execute: backup, bump pinned version, apply migrations, verify.
- **UPGR-002** · A failed `upgrade` must leave the operator with the pre-upgrade backup and a working path back to the pre-upgrade version.
- **UPGR-003** · Schema migrations must run during `upgrade` and at no other time.

## DRIL — rehearsal

- **DRIL-001** · `drill` must restore the most recent backup to a scratch target, boot the full stack, run a smoke CI job, and report success or the specific failing step.
- **DRIL-002** · A drill instance must be quarantined: outbound webhooks and email disabled, DNS untouched, reachable only through an SSH tunnel.
- **DRIL-003** · `drill` must leave the scratch target clean on completion.

## STAT — status

- **STAT-001** · `status` must report instance health (services up, TLS validity, disk headroom) and last-backup age.
- **STAT-002** · `status` must report replication lag for golden-path transports and must identify operator-assembled transports as unmeasured.

## API — local control API

- **API-001** · The API must bind loopback by default and expose RPC verbs for every operation.
- **API-002** · Mutation verbs must return a job ID, with progress streamed over SSE from the CORE-002 event model.

## UI — dashboard

- **UI-001** · `ui` must serve the dashboard on loopback and open the operator's browser.
- **UI-002** · The dashboard must cover status, replication lag, backup history, drill results, and promotion, each backed by the same core operations as the CLI.

## XCUT — cross-cutting

- **XCUT-001** · Every operation must work from any machine holding the bundle and key access; nothing may depend on the machine that ran `init`.
- **XCUT-002** · The CLI must render the CORE-002 event stream in the terminal for every long-running operation.
