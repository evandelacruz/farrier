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
  importrepo/         repository migration from GitHub/GitLab via Forgejo's migrate API (IMPT-001)
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
repos/                    bare repositories, tar-captured post push-hold
blobs/                    LFS objects, CI artifacts, avatars (via blob adapter)
keys/                     bundle key material
```

- Capture order: hold pushes → SQLite online backup → tar bare repos → release pushes → blob capture → checksum → encrypt → verify → write.
- Verification at creation and at restore runs the same code path: manifest completeness, checksums, cross-consistency (DB repo/blob references resolve).

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

## Deployment (`up`, UP-001, UP-002)

`internal/core/deploy.Up` is the sequencing over orchestrate, forge, caddy, and acme that a real deployment needs, given only an `ssh://user@host` target and a loaded bundle:

1. Check Docker is reachable (ORCH-001's `CheckHost`).
2. Resolve the bundle's key material through its keystore driver and render `app.ini` (FORGE-001), ship it to the host, and add a bind mount for it to the forgejo service's Compose definition (`orchestrate.WithBindMount`) — deploy-time content, never written into the bundle directory (KEY-003).
3. Issue a TLS certificate for the bundle domain via ACME DNS-01 (`acme.Issue`, ACME-001), using the DNS-01 provider name the manifest carries from `init`'s own zone-control proof (`Manifest.ACME`). The account key is generated fresh for this issuance — INIT-003, which would persist one as durable bundle key material, has not landed. Render the Caddyfile (`caddy.RenderCaddyfile`) that terminates TLS with the issued certificate and reverse-proxies to Forgejo, ship the Caddyfile and certificate to the host, bind-mount them into the caddy service, and publish its HTTPS port (`orchestrate.WithPorts`) — UP-002.
4. Converge the host to that Compose definition (ORCH-002).
5. Wait for the forgejo container to accept `docker compose exec`, since `up -d` returning doesn't guarantee the entrypoint has finished.
6. Provision the first admin account (FORGE-002).
7. Wait for the caddy container to accept `docker compose exec` the same way, so `up` returns only once the forge is actually serving HTTPS.

Pointing the bundle domain's DNS at the deploy host is the operator's own
topology, arranged the same way they arrange the host itself (spec.md "What
the operator owns") — `up` does not manage DNS records.

Every step reports through the job's CORE-002 event stream; `deploy.Up` owns the job's terminal event. `cmd/farrier up` is the CLI skin: it connects over SSH, calls `deploy.Up`, and prints the same events a dashboard would render over SSE.

## Repository import (IMPT-001)

`internal/core/importrepo.Run` wraps Forgejo's own repository migration
API rather than reimplementing git/LFS transfer (spec.md "Importing
repositories"): given a source URL and a source access token, it calls
`POST /api/v1/repos/migrate` against the bundle's own forge (`https://` +
the manifest's domain — the address `up` leaves serving HTTPS, UP-002),
using Forgejo's stable `MigrateRepoOptions` wire format.

- **Inputs:** the destination forge's API token, the source repository's
  clone URL, and the source token. Source service (`github` or `gitlab`)
  is inferred from the URL's host unless given explicitly; destination
  owner and repository name default to the API token's own account and
  the URL's last path segment.
- **What migrates:** code, full history, and LFS objects (`lfs: true`),
  the default branch (carried automatically by a git migration), no
  continuous mirror (`mirror: false` — IMPT-002 adds that). Issues, pull
  requests, wiki, milestones, labels, and releases are left at their zero
  value so none of that migrates; that history stays on the source forge.
- **Not yet implemented:** continuous mirror sync (IMPT-002) and
  multi-repository batch reporting with all-or-nothing per-repository
  registration (IMPT-003) — later additions to the same command.
- `cmd/farrier import` is the CLI skin: it loads the bundle for its
  domain, calls `importrepo.Run`, and prints the same job event stream a
  dashboard would render over SSE.

## API

- Loopback HTTP, default `127.0.0.1:7433`.
- RPC verbs: `POST /init`, `POST /up`, `POST /backup`, `POST /restore`, `POST /promote`, `POST /upgrade`, `POST /drill`, `POST /import`, `GET /status`.
- Mutation verbs return `{ jobId }`; `GET /jobs/{id}/events` streams progress over SSE.
- One event schema for all operations: `{ jobId, step, state, detail, timestamp }`. The CLI renders the same events in the terminal; the dashboard renders them in the browser.

## Operational targets

- **RTO:** promote completes — snapshot pulled, restored, verified, services live, DNS flipped — within 10 minutes on the reference instance (10 GB state, 100 Mbps between backup target and new host).
- **RPO:** equals backup cadence; the golden-path cron documents hourly.
- **Backup impact:** push-hold under 10 seconds on the reference instance; reads uninterrupted.
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
