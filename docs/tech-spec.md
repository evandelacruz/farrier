# Tech Spec

How the system is built: internal structure, formats, protocols, and operational targets. The decisions this structure serves are recorded in [spec.md](spec.md); the observable behavior it must produce is in [functional-requirements.md](functional-requirements.md).

## Repository layout

Go module, single binary output.

```
cmd/farrier/          CLI entrypoint (thin: flag parsing → core calls)
internal/core/        the engine — all logic lives here
  bundle/             manifest, bundle directory, version pins
  state/              the four state kinds and their export interfaces
  backup/             snapshot creation, encryption, verification
  restore/            snapshot verification, rebuild, identity install
  orchestrate/        SSH transport, Compose rendering and execution
  forge/              Forgejo configuration, admin bootstrap, CI reconciliation
  acme/               cert issuance and renewal (lego)
  driver/             exec-based driver protocol (JSON on stdin/stdout), shared by dns/, keystore/, blob/
  dns/                DNS driver interface + shipped drivers
  keystore/           keystore driver interface + shipped drivers
  blob/               blob adapter interface + shipped adapters
  events/             the job/progress event model
internal/api/         loopback HTTP server, RPC endpoints, SSE
web/                  dashboard (embedded into the binary via go:embed)
docs/                 this documentation
```

## Bundle directory

A plain directory, designed to live in a private git repo.

```
farrier.yaml          manifest: domain, pinned image digests, driver config,
                      state-kind declarations, checksum algorithm
compose/              rendered Docker Compose definitions
```

- Manifest format: YAML.
- Versions are pinned by image digest, not tag.
- Key material is referenced by keystore driver config, never stored.

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
- **Keystore:** `Resolve(keyName) → secret`. Shipped: `file`, `command`.
- **Blob:** `List`, `Get`, `Put`, streaming. Shipped: `local`, `s3`.

ACME DNS-01 uses lego's own provider set and is independent of the DNS driver interface.

### Keystore driver config

- **`file`** (`config.path`): a local directory. `Resolve(keyName)` reads
  `path/keyName` and returns its bytes verbatim — one file per piece of key
  material.
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
- Fork-PR approval requirement is set on by default in rendered configuration.
- CI reconciliation at promote: a direct SQLite update resetting `running` → `queued` in the actions tables, executed before services start.

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
