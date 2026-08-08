# Tech Spec

How the system is built: internal structure, formats, protocols, and operational targets. The decisions this structure serves are recorded in [spec.md](spec.md); the observable behavior it must produce is in [functional-requirements.md](functional-requirements.md).

## Repository layout

Go module, single binary output.

```
cmd/farrier/          CLI entrypoint (thin: flag parsing → core calls)
internal/core/        the engine — all logic lives here
  bundle/             manifest, bundle directory, version pins
  initialize/         builds a bundle from a domain and driver targets (INIT-001)
  state/              the four state kinds and their export interfaces
  backup/             snapshot creation, encryption, verification
  restore/            snapshot verification, rebuild, identity install
  orchestrate/        SSH transport, Compose rendering and execution
  forge/              Forgejo configuration, admin bootstrap, CI reconciliation
  deploy/             `up` sequencing: check host, ship config, issue TLS cert, converge, bootstrap admin
  status/             instance health: services up, TLS validity, disk headroom, last-backup age (STAT-001)
  importer/           `import` sequencing: calls Forgejo's migration API, with optional mirror sync (IMPT-001, IMPT-002)
  acme/               cert issuance and renewal (lego)
  caddy/              Caddy config rendering: the Caddyfile that terminates TLS with a core-issued certificate
  driver/             exec-based driver protocol (JSON on stdin/stdout), shared by dns/, keystore/, blob/
  dns/                DNS driver interface + shipped drivers
  keystore/           keystore driver interface + shipped drivers
  blob/               blob adapter interface + shipped adapters
  registry/           resolves a container image reference to its digest (init's image pinning)
  events/             the job/progress event model
internal/api/         loopback HTTP server, RPC endpoints, SSE
web/                  dashboard (embedded into the binary via go:embed)
docs/                 this documentation
```

## Bundle directory

A plain directory, designed to live in a private git repo.

```
farrier.yaml          manifest: domain, pinned image digests, driver config,
                      ACME DNS-01 config, state-kind declarations, checksum
                      algorithm
compose/              rendered Docker Compose definitions
```

- Manifest format: YAML.
- Versions are pinned by image digest, not tag.
- Key material is referenced by keystore driver config, never stored.

## Bundle creation (INIT-001, INIT-002, INIT-003)

`internal/core/initialize.Run` builds and writes a bundle: it validates the
domain and the keystore target, proves control of the domain's DNS zone via
an ACME DNS-01 challenge, generates and stores every piece of bundle key
material, resolves every component's image reference to a digest via
`internal/core/registry`, renders Compose (ORCH-002), and saves the bundle
(CORE-001). Every step emits a CORE-002 job event, so `farrier init` and a
future dashboard render the same progress.

- **Required:** domain, a keystore target (driver + config), a blob target
  (driver + config), an ACME DNS-01 provider name — Manifest.Validate
  requires the first three before a bundle can be saved; the DNS-01
  provider is Run's own precondition for the zone-control proof. The
  keystore target's driver must also implement `keystore.Writer` (below) —
  validate fails immediately, before any ACME exchange, when it doesn't.
- **Zone-control proof (INIT-002):** Run generates a fresh ACME account key
  and runs a full ACME DNS-01 exchange through `internal/core/acme.Issue`
  (ACME-001) against the named lego DNS-01 provider — independent of the
  bundle's own DNS driver (`internal/core/dns`). The provider reads its
  credentials from the process environment, the way the operator already
  runs any lego-based tool; Run neither reads nor sets them. Failure aborts
  `init` before any key generation, image resolution, or bundle write,
  naming the reason.
- **Key-material generation (INIT-003):** a successful proof exchange also
  yields a real certificate for the domain — Run persists it instead of
  issuing a second one, so `init` runs exactly one ACME DNS-01 exchange
  against the operator's provider. `internal/core/initialize.generateKeyMaterial`
  builds the rest: 32 random bytes (base64, unpadded, URL-safe) each for
  Forgejo's `SECRET_KEY`, `INTERNAL_TOKEN`, and LFS JWT secret (the names
  `forge.KeySecretKey`, `forge.KeyInternalToken`, `forge.KeyLFSJWTSecret`
  own, since `forge` is what resolves and consumes them at deploy time), a
  fresh ed25519 SSH host key pair (OpenSSH PEM private key, authorized-keys
  public key), and a fresh age identity (`filippo.io/age`) for backup
  encryption. Every piece is stored through the keystore driver's `Store`
  method, in a fixed order, aborting the whole `init` on the first failure —
  key generation is all-or-nothing, the same as zone-control proof.
- **Keystore write support:** `keystore.Writer` (`Store(ctx, keyName,
  secret) error`) is the write side of a keystore `Driver`, additive to the
  read-only interface tech-spec's Driver interfaces section documents.
  `FileDriver` implements it — its storage is an unambiguous directory on
  disk, so `Store` just writes `path/keyName` (creating `path` if needed)
  and refuses to overwrite a key that already has content, so a second
  `init` run against an already-populated keystore target fails loudly
  instead of silently rotating bundle identity. `CommandDriver`
  deliberately does not implement `Writer`: KEY-002 defines it as reading
  the stdout of an operator-specified command, with no generic notion of
  "write a secret here." `initialize.Run` discovers write support with a
  type assertion and fails, naming the driver, when it's missing —
  `init` against a command-backed keystore requires the operator to
  provision key material there some other way first.
- **Images:** `forgejo` and `caddy` default to their `:latest` tag on their
  canonical registry (`codeberg.org/forgejo/forgejo`, `docker.io/library/caddy`)
  and can be overridden per component. Every reference, default or override,
  is resolved to `name@sha256:...` before it reaches the manifest — the
  registry package speaks the standard OCI/Docker distribution API
  (anonymous Bearer challenge included), so this works against any
  registry, not just the two shipped defaults.

## State export interfaces

Each of the four state kinds (spec.md "The four kinds of state") gets an
export interface in `internal/core/state`, consumed by both `backup/` and
the operator's own replication tooling.

- **Git (STATE-001):** `GitExporter.Remotes` lists the bare repositories
  under a Forgejo repository root — `<root>/<owner>/<repo>.git`, Forgejo's
  own layout — and returns each as a `Remote{Name, URL}` ready for
  `git remote add --mirror` / `git push --mirror`. `LocalGitExporter` walks
  the root directly (the drill path, DRIL-001); `SSHGitExporter` lists it
  over an already-connected `Runner` (`*orchestrate.Client` in production)
  and returns `ssh://` URLs.
- **Database (STATE-002):** `DatabaseExporter.Snapshot` returns a stream
  over a point-in-time copy of Forgejo's SQLite database, taken with the
  `sqlite3` CLI's `.backup` meta-command — the shell-facing name for
  SQLite's Online Backup API, so the source database is never paused.
  `LocalDatabaseExporter` runs `sqlite3` directly against a database file
  on disk (the drill path, DRIL-001). `SSHDatabaseExporter` runs it inside
  the Forgejo container over an already-connected `Runner`, via
  `docker exec` — ORCH-001 guarantees only Docker and SSH on the host, not
  a bare `sqlite3` install, so the backup runs where `sqlite3` is actually
  known to exist: inside the pinned Forgejo image.
- **Key material (STATE-004):** `KeyExporter.Names` enumerates the fixed
  set of key names a bundle carries — the three Forgejo secrets
  (`forge.KeySecretKey`, `forge.KeyInternalToken`, `forge.KeyLFSJWTSecret`),
  the TLS certificate chain and its private key, and the SSH host key —
  since none of the shipped keystore drivers (file, command, exec) can list
  what they hold; `KeyExporter.Resolve` reads one of those names from a
  `keystore.Driver`. `KeystoreKeyExporter` is the one implementation: every
  keystore driver already resolves by name, so no Local/SSH split is
  needed the way git and database exporters have. The age backup key is
  not part of this set — it encrypts the backup rather than travels inside
  one, and the operator holds it directly (spec.md "Key custody").

## Snapshot format

One age-encrypted archive per backup:

```
snapshot-manifest.json    forgejo version, timestamp, per-component checksums
db.sqlite                 SQLite online-backup output
repos/                    <name>.refs.tar (ref state, pinned during the hold) and <name>.tar (objects, tarred after release)
blobs/                    LFS objects, CI artifacts, avatars (via blob adapter)
keys/                     bundle key material
```

- Capture order: hold pushes → SQLite online backup → record every repository's refs → release pushes → tar objects → blob capture → checksum → encrypt → verify → write.
  Git objects are immutable and append-only; only refs move. Recording each repository's ref state (`HEAD`, `packed-refs`, `refs/`) is a few KB and instant, so it's the only git work that has to happen inside the hold — the object tar, which scales with git data, happens afterward, outside it. A push landing during that tar can only add objects; it can never disturb a ref already pinned during the hold. This keeps the hold database-only: a second or two, regardless of how much git data the instance holds.
- During the hold, Caddy rejects git pushes outright with an explicit message — it does not queue or buffer them, so a client mid-push gets a clean, immediate failure and retries. Reads and fetches are untouched throughout.
- The hold releases on every exit path — success, error, panic, or a canceled context — so a capture that dies mid-hold can't leave pushes rejected until an operator notices. A configurable ceiling (low default) backstops the release against a wedged capture; it's a bug backstop, not a growth alarm, since nothing about it scales with instance size.
- Verification at creation and at restore runs the same code path: manifest completeness, checksums, cross-consistency (DB repo/blob references resolve).

## Snapshot capture (BKUP-001, BKUP-002)

`internal/core/backup.Run` captures the four state kinds — via the
`state.DatabaseExporter`, `state.GitExporter`, `state.BlobExporter`, and
`state.KeyExporter` interfaces (STATE-001–004) — into a plain snapshot
directory and writes `snapshot-manifest.json`: the Forgejo version, the
capture timestamp, the checksum algorithm, and one checksummed `Component`
per captured file (one database, one per blob, one per key, and two per
repository — refs and objects). Every step emits a CORE-002 job event.

`state.GitExporter` only enumerates remotes (STATE-001); it doesn't stream
repository content, since replication is ordinarily the operator's own
mirroring tooling. `backup.Run` pairs it with a `backup.GitCapturer`
(`LocalGitCapturer`, `SSHGitCapturer`) that tars each bare repository the
exporter lists — the same Local/SSH split `state.GitExporter` and
`state.DatabaseExporter` already use. `GitCapturer.Refs` tars only the
mutable ref paths (`HEAD`, `packed-refs`, `refs/`); `GitCapturer.Archive`,
unchanged from BKUP-001, tars the full object store.

`backup.PushHold` (`Engage`/`Release`) is the push-hold mechanism: `Run`
engages it before the database backup and ref recording, and releases it
before the object tar. `backup.CaddyPushHold` implements it by reloading
the bundle's already-running Caddy — over the same `state.Runner` SSH
seam `SSHDatabaseExporter` and `SSHGitCapturer` use — against a temporary
Caddyfile (`caddy.RenderPushHoldCaddyfile`) that returns 503 for git's
smart-HTTP push endpoints (`POST .../git-receive-pack`,
`GET .../info/refs?service=git-receive-pack`), then reloads back to the
original, untouched Caddyfile at `caddy.ConfigPath` to release.
`backup.NoopPushHold` covers topologies with no proxy in front of git
traffic (a local capture, a drill).

`Run` produces the plain, unencrypted snapshot the rest of this section's
pipeline builds on: encryption (BKUP-003), verification at creation
(BKUP-004), and writing the result to an S3-compatible URI or filesystem
path (BKUP-005) are separate, not yet implemented.

## Driver interfaces

All three follow one posture: a Go interface for in-tree drivers, plus an exec-based protocol for out-of-tree ones. The exec protocol itself is generic and lives once, in `internal/core/driver` (CORE-003): `driver.Exec` runs an executable once per call, writing `{"method", "params"}` as a `Request` to its stdin and reading `{"ok", "result", "error"}` back as a `Response` from its stdout — one process per call, no long-lived session. `driver.Exec` satisfies `driver.Invoker`, the seam each driver-type package wraps behind its own domain interface and its own method names.

- **DNS:** `Set(record, value, ttl)`, `Delete(record)`. Shipped: `cloudflare`, `rfc2136`.
- **Keystore:** `Resolve(keyName) → secret`, every driver; `Store(keyName,
  secret) → error` on `file` only (INIT-003's generated key material has
  nowhere else defined to land). Shipped: `file`, `command`.
- **Blob:** `List`, `Get`, `Put`, streaming. Shipped: `local`, `s3`.

ACME DNS-01 uses lego's own provider set and is independent of the DNS driver interface.

### Keystore driver config

- **`file`** (`config.path`): a local directory. `Resolve(keyName)` reads
  `path/keyName` and returns its bytes verbatim — one file per piece of key
  material. `Store(keyName, secret)` writes the same file, creating `path`
  if needed, and refuses to overwrite a key that already has content.
- **`command`** (`config.command`): one shell command, run via `sh -c`.
  `Resolve(keyName)` sets `FARRIER_KEY_NAME` in the command's environment
  and returns its trimmed stdout — one command branches on the env var to
  serve every key the bundle needs.
- Any other driver name resolves through the CORE-003 exec protocol:
  `config.path` is the executable, `config.args` its fixed arguments;
  method `resolve`, params `{"key": keyName}`, result `{"secret":
  "<base64>"}`.

## Orchestration

- Transport: SSH (Go `x/crypto/ssh`), authenticated by the operator's existing SSH agent (`SSH_AUTH_SOCK`) or an explicit key file — no other auth path, no password prompts.
- Host identity is checked against the operator's `known_hosts` (default `~/.ssh/known_hosts`); an unrecorded or mismatched host key fails the connection rather than prompting to trust it, since jobs run unattended.
- Host readiness beyond SSH itself is a single check: Docker reachable over the same SSH session (`docker version`). Farrier requires nothing else of the host.
- The CLI renders Compose files from the manifest, ships them to the host, and drives `docker compose` over the SSH session.
- Host state is treated as disposable: `up` converges the host to the bundle definition idempotently; drift is overwritten.

## Forge configuration

- Forgejo `app.ini` is fully rendered from the manifest — the install wizard is pre-answered by configuration.
- Admin bootstrap runs `forgejo admin user create` post-start; credentials are emitted once through the event stream.
- Rendered `app.ini` enables Actions (`[actions] ENABLED = true`, inlined in `internal/core/forge.RenderAppINI`). Forgejo's fork-PR approval gate is unconditional once Actions is on — it exposes no app.ini or per-repository key to loosen it — so enabling Actions is what the requirement needs.
- CI reconciliation at promote: a direct SQLite update resetting `running` → `queued` in the actions tables, executed before services start.

## Deployment (`up`, UP-001, UP-002, UP-003)

`internal/core/deploy.Up` is the sequencing over orchestrate, forge, caddy, and acme that a real deployment needs, given only an `ssh://user@host` target and a loaded bundle:

1. Check Docker is reachable (ORCH-001's `CheckHost`).
2. Resolve the bundle's key material through its keystore driver and render `app.ini` (FORGE-001), ship it to the host, add a bind mount for it to the forgejo service's Compose definition (`orchestrate.WithBindMount`) — deploy-time content, never written into the bundle directory (KEY-003) — and set a checksum of that rendered `app.ini` as an environment variable on the same service (`orchestrate.WithEnv`).
3. Issue a TLS certificate for the bundle domain via ACME DNS-01 (`acme.Issue`, ACME-001), using the DNS-01 provider name the manifest carries from `init`'s own zone-control proof (`Manifest.ACME`). The account key is generated fresh for this issuance, and so is the certificate — every call to `up` re-issues, regardless of the certificate `init` already persisted as bundle key material (INIT-003; see "Known gap" below). Render the Caddyfile (`caddy.RenderCaddyfile`) that terminates TLS with the issued certificate and reverse-proxies to Forgejo, ship the Caddyfile and certificate to the host, bind-mount them into the caddy service, and publish its HTTPS port (`orchestrate.WithPorts`) — UP-002.
4. Converge the host to that Compose definition (ORCH-002).
5. Wait for the forgejo container to accept `docker compose exec`, since `up -d` returning doesn't guarantee the entrypoint has finished.
6. Provision the first admin account (FORGE-002).
7. Wait for the caddy container to accept `docker compose exec` the same way, so `up` returns only once the forge is actually serving HTTPS.

Pointing the bundle domain's DNS at the deploy host is the operator's own
topology, arranged the same way they arrange the host itself (spec.md "What
the operator owns") — `up` does not manage DNS records.

Every step reports through the job's CORE-002 event stream; `deploy.Up` owns the job's terminal event. `cmd/farrier up` is the CLI skin: it connects over SSH, calls `deploy.Up`, and prints the same events a dashboard would render over SSE.

Re-running `up` against a host it has already deployed to converges the host to the current bundle definition, and is safe except for the TLS step (UP-003 — `partial`; see "Known gap" below):

- Steps 1, 5, and 7 are read-only probes.
- Step 2 always re-ships the current `app.ini` and re-derives its checksum. `docker compose up -d` (step 4) decides whether to recreate a service by diffing its resolved config — image, environment, volumes, labels — never the bytes of a file a bind mount happens to point at, so without the checksum a content-only `app.ini` change would ship to disk without the running container ever picking it up. Carrying the checksum as an environment variable puts that content into the diff `docker compose` already does.
- Step 4 is idempotent by construction: `Converge` always ships the full Compose definition and replaces the remote directory wholesale, so `docker compose up -d --remove-orphans` reconciles from scratch on every call.
- Step 6 treats "the admin account already exists" (Forgejo's `admin user create` failing on a duplicate username) as done, not a failure, and does not re-emit or reuse the fresh password it generated for that call — the account already has its original password, handed to the operator on the run that actually created it.

**Known gap:** step 3 is not yet re-run-safe. `configureTLS` re-issues a certificate from a fresh ACME account on every call, which risks Let's Encrypt's duplicate-certificate rate limit (about 5 certificates with identical SANs per week) after a handful of re-runs. The read side to close this exists: the certificate `init` persists (INIT-003, `state.KeyTLSCertificate`/`state.KeyTLSPrivateKey`) and the renewal-aware `acme.EnsureValid` (ACME-002) that can decide against that persisted certificate whether a new one is actually due. What's missing is the write side — a renewed certificate has to be persisted back to the keystore, or the next `up` re-issues again — and `keystore.Writer.Store` refuses to overwrite a key that already has content by design, the same invariant that keeps `SECRET_KEY`, `INTERNAL_TOKEN`, and the SSH host key from silently rotating (spec.md "Identity"). Whether, and how, a renewal-eligible key like the TLS certificate gets a distinct update path without loosening that guarantee for keys that must never rotate silently is an open decision, not made in this PR. UP-003 stays `partial` in `docs/status.json` until it is.

## Status (`status`, STAT-001)

`internal/core/status.Check` is a synchronous read, not a job (tech-spec.md
"API": `GET /status` returns directly, no `jobId`) — it reflects the
instance's current state each time it's called, never a cached one:

1. **Services up:** `docker compose ps --all --format json` inside the
   deployed project (`orchestrate.ComposeCommand`'s cd and
   `COMPOSE_PROJECT_NAME`/`COMPOSE_FILE` prefix, the same one
   `deploy`'s `composeRunner` builds for `forge.Bootstrap`), checked
   against the two services `up` deploys today (`forge.Service`,
   `caddy.Service`). A service whose container state isn't `"running"`, or
   that has no container at all, reports down with docker compose's own
   status string as detail.
2. **TLS validity:** resolves `state.KeyTLSCertificate` through the
   bundle's keystore driver and parses it as X.509 — valid when the host
   clock falls inside the certificate's validity window, expiring-soon at
   the same 14-day threshold ACME-002 sets for renewal warnings
   (`status.CertExpiryWarning`).
3. **Disk headroom:** `df -Pk` on the host, default path `/` — no landed
   decision yet pins forge state to a specific bind-mounted host
   directory, so root is the only host-wide signal available without
   guessing one.

`status.Check` returns an error naming which of the three failed rather
than a partially-filled report — consistent with the rest of the core's
"fail loudly, name the reason" posture (`ORCH-001`'s `CheckHost`,
`BKUP-004`). `cmd/farrier status` is the CLI skin: it connects over SSH,
calls `status.Check`, and prints the report; its exit code reflects
whether the report could be produced, not whether the instance it
describes is healthy.

**Last-backup age is not implemented yet.** STAT-001 also requires it, but
finding the most recent snapshot needs a stable convention for what
`backup` (BKUP-001..005, not yet landed) writes to its destination and how
`status` locates it there — `backup`'s destination is a per-invocation
`--to` flag (spec.md "Golden path"), not bundle-persisted config, and
`blob.Object` (BLOB-001/002) carries no timestamp today. That convention
belongs to backup's own design, not something `status` should invent
ahead of it; `docs/status.json` carries STAT-001 as partial with this as
the remaining slice.

## Importing repositories (`import`, IMPT-001, IMPT-002, IMPT-003)

`internal/core/importer.Run` calls Forgejo's own migration endpoint, `POST /api/v1/repos/migrate`, on the target instance's API — no git transport is reimplemented:

1. Resolve the source service (`github` or `gitlab`) from the source URL's host, or take it from an explicit override for self-hosted sources the host doesn't name.
2. Derive the target repository name from the source URL's last path segment unless one is given explicitly.
3. Call `POST /api/v1/repos/migrate` with `clone_addr`, `auth_token` (the source token), `repo_owner`, `repo_name`, `lfs: true` (IMPT-001's LFS objects), and `private`. Issue, pull-request, wiki, and release history are always requested `false` — spec.md "Importing repositories" leaves that history on the source forge unconditionally, so it is never a caller option.
4. When `Mirror` is set (IMPT-002), the same request also carries `mirror: true` and, if given, `mirror_interval` — Forgejo re-pulls the source on that cadence with no further Farrier involvement.

The default branch travels automatically as part of the git migration; there is no separate step for it. `Run` owns the job's terminal event the way `deploy.Up` does. `cmd/farrier import` is the CLI skin: `-target` addresses the Farrier instance and `-source` the origin, `-owner`/`-name` set the repository's home on the target, and `-mirror`/`-mirror-interval` opt into continuous sync. The two API tokens are never CLI flags — `FARRIER_TARGET_TOKEN` and `FARRIER_SOURCE_TOKEN` in the process environment, read the same way `init`'s ACME DNS-01 provider reads its own credentials (see "Bundle creation" above), so neither token is ever visible in `ps` output or shell history.

**IMPT-003 — batch reporting and no partial repository on failure.** `importer.RunBatch` migrates many repositories against one target instance within a single job: each repository gets its own `migrate:<n>` step in the event stream and its own entry in the returned `BatchResult` (source, `Result`, and `error`, independently), and the batch keeps going past one repository's failure so the rest still import. The job's own terminal event reflects the batch as a whole — succeeded only if every repository succeeded — but per-repository detail lives in the step events and `BatchResult`, not in that one terminal event. `cmd/farrier import -file <manifest.yaml>` is the batch CLI skin: the manifest lists repositories only (`source`, and optionally `service`, `owner`, `name`, `private`, `mirror`, `mirrorInterval` per entry) and never credentials, which stay in `-target`/`-owner`/`-private`/`-mirror`/`-mirror-interval` and the two environment tokens, applied as the batch-wide default for any entry that doesn't set its own. `-file` and `-source` are mutually exclusive.

Whether run singly or as a batch, a failed migration leaves no partially-registered repository on the target: `migrate`'s failure paths (a non-2xx response, or the request itself failing) call `DELETE /api/v1/repos/{owner}/{repo}` best-effort on a detached, timed-out context, so cleanup still runs even when the failure was the caller's own context expiring or being canceled. A 404 from that delete (nothing was ever registered) is not an error; a genuine cleanup failure is reported alongside, never in place of, the original migration error, since it means the operator may need to remove that repository by hand. A response that decodes successfully after a 2xx is a real, complete repository — decode failures past that point are a client-side bug, not a partial registration, and are never cleaned up.

## API

- Loopback HTTP, default `127.0.0.1:7433`.
- RPC verbs: `POST /init`, `POST /up`, `POST /backup`, `POST /restore`, `POST /promote`, `POST /upgrade`, `POST /drill`, `POST /import`, `GET /status`.
- Mutation verbs return `{ jobId }`; `GET /jobs/{id}/events` streams progress over SSE.
- One event schema for all operations: `{ jobId, step, state, detail, timestamp }`. The CLI renders the same events in the terminal; the dashboard renders them in the browser.

## Operational targets

- **RTO:** promote completes — snapshot pulled, restored, verified, services live, DNS flipped — within 10 minutes on the reference instance (10 GB state, 100 Mbps between backup target and new host).
- **RPO:** equals backup cadence; the golden-path cron documents hourly.
- **Backup impact:** push-hold is database-only (SQLite online backup plus recording every repository's ref state) and stays a second or two regardless of git data size, since the object tar runs outside it; reads and fetches are uninterrupted throughout.
- **Forge host floor:** 2 vCPU, 2 GB RAM, Linux x86_64 or arm64, Docker ≥ 24.
- **Control plane:** macOS and Linux; single static binary per platform.
- **DNS TTL:** 60 seconds on all bundle records.
- **Cert renewal:** lego renews at two-thirds of cert lifetime; `status` warns when a cert is inside 14 days of expiry.
- **Drill cost:** a drill runs on any scratch Docker host, including the operator's own machine via local containers.

## Security posture

- API binds loopback; any wider exposure is operator topology.
- Key material is held in memory only during operations; never written to logs, event streams, or the bundle directory.
- Snapshots are age-encrypted before leaving the forge host; the operator holds the sole key.
- CI executes in containers; the trust boundary and fork-PR policy are recorded in [spec.md](spec.md).
