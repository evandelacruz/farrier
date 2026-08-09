# Tech Spec

How the system is built: internal structure, formats, protocols, and operational targets. The decisions this structure serves are recorded in [spec.md](spec.md); the observable behavior it must produce is in [functional-requirements.md](functional-requirements.md).

**Scope.** This document describes things no single package owns: the repository's shape, on-disk and on-the-wire formats, the driver protocols third parties implement against, and the numbers the system is held to. It deliberately does **not** narrate how each requirement is implemented. That belongs in the package's own doc comments, next to the code, where it cannot drift — and this file exists to be read by someone deciding where new code goes, not by someone re-deriving what existing code does.

## Repository layout

Go module, single binary output.

```
cmd/farrier/          CLI entrypoint (thin: flag parsing → core calls)
internal/core/        the engine — all logic lives here
  bundle/             manifest, bundle directory, version pins
  initialize/         builds a bundle from a domain and driver targets
  state/              the four state kinds and their export interfaces
  backup/             snapshot capture, encryption, verification, destination write
  restore/            snapshot verification, rebuild, identity install
  promote/            failover sequencing: restore, reconcile CI, flip DNS
  upgrade/            upgrade sequencing: health gate, backup, bump version, converge
  drill/              rehearsal sequencing: restore the latest snapshot to a
                      scratch target, boot it, report the failing step
  orchestrate/        SSH transport, Compose rendering and execution
  forge/              Forgejo configuration, admin bootstrap, CI reconciliation
  deploy/             `up` sequencing, and the host state layout below
  status/             instance health, last-backup age, replication lag
  importer/           `import` sequencing over Forgejo's migration API
  acme/               cert issuance and renewal (lego)
  caddy/              Caddyfile rendering: TLS termination with a core-issued cert
  driver/             exec-based driver protocol (JSON on stdin/stdout)
  dns/                DNS driver interface + shipped drivers
  keystore/           keystore driver interface + shipped drivers
  blob/               blob adapter interface + shipped adapters
  registry/           resolves a container image reference to its digest
  events/             the job/progress event model
  ui/                 serves the dashboard and the API on one loopback
                      listener, and opens the operator's browser
internal/api/         loopback HTTP server, RPC endpoints, SSE
web/                  dashboard (embedded into the binary via go:embed)
  assets/             the page as served: static HTML, CSS, and JS, no toolchain
tools/                agent orchestration — dev-only, never in the product binary
docs/                 this documentation
```

Every operator command is one core package sequencing others, with `cmd/farrier` and `internal/api` as thin skins over it. Both skins emit the same CORE-002 event stream, and the sequencing package owns the job's terminal event.

## Bundle directory

A plain directory, designed to live in a private git repo.

```
farrier.yaml          manifest: domain, pinned image digests, driver config,
                      ACME DNS-01 config, CI runner config, state-kind
                      declarations, checksum algorithm
compose/              rendered Docker Compose definitions
```

- Manifest format: YAML.
- Versions are pinned by image digest, not tag.
- `actions.colocatedRunner` is the CI runner config: `false` keeps the bundled Actions runner off the forge host, and the operator registers a remote runner against the bundle domain instead (spec.md "CI trust boundary"). Absent means enabled.
- Key material is referenced by keystore driver config, never stored.
- Driver config paths are absolute. The manifest carries them as literal strings re-resolved on every call, so a relative path would silently follow whatever directory a later command ran from (XCUT-001).

## Snapshot format

One age-encrypted archive per backup:

```
snapshot-manifest.json    forgejo version, timestamp, per-component checksums
db.sqlite                 SQLite online-backup output
repos/                    <name>.refs.tar (ref state, pinned during the hold)
                          <name>.tar (objects, tarred after release)
blobs/                    LFS objects, CI artifacts, avatars (via blob adapter)
keys/                     bundle key material
```

**Capture order:** hold pushes → SQLite online backup → record every repository's refs → release pushes → tar objects → blob capture → checksum → encrypt → verify → write.

Two properties of that order are load-bearing and settled:

- **The hold is database-only.** Git objects are immutable and append-only; only refs move. Recording each repository's ref state is a few KB, so it is the only git work inside the hold — the object tar, which scales with git data, runs after release. A push landing during that tar can only add objects; it can never disturb a ref already pinned. The hold therefore stays a second or two regardless of instance size.
- **Verify runs after encrypt**, against the decrypted form of the exact bytes about to be written — not against the pre-encryption directory alone. Verification at creation and at restore run the same code path: manifest completeness, checksums, and cross-consistency (database references to repos and blobs resolve).

## Host state layout

Forge state lives on the host, under `<RemoteDir>/state`, bind-mounted into the container that serves it:

```
<RemoteDir>/state/git      → forgejo:/data/git/repositories
<RemoteDir>/state/gitea    → forgejo:/data/gitea   (database, LFS, attachments,
                                                    avatars, CI artifacts, and the
                                                    bundle's SSH host key)
<RemoteDir>/state/blobs    created, not mounted — reserved for a `local` blob
                           adapter; no container reads it today
<RemoteDir>/state/forgejo-version
                           the Forgejo image this state has been started
                           under; one line, not mounted
```

`forgejo-version` is what makes UPGR-003 checkable. Forgejo migrates its schema whenever it starts on a version newer than the database it opens, so migrations are decided entirely by which image starts against which state. `deploy.Up` records the image it is about to start here, before starting it, and refuses to start any other image against state this file already names — the exemption is `upgrade`, which has taken a backup and gated on health by then. `restore` stamps the snapshot's own version alongside the database it places, so the version it then boots (spec.md "Version pinning") matches by construction. It is a property of this host's state, not of the bundle, which is why it lives here: two hosts restored from the same bundle can legitimately carry different values. An absent file means *unknown* — a fresh host, or one deployed before this record existed — and is not treated as a migration.

This is what makes spec.md's stateless/stateful split real. Containers are the disposable half and are recreated on any config change, so no stateful kind may live inside one. `<RemoteDir>/state` is the one directory on the host that is not disposable.

`deploy.GitStatePath`, `deploy.GiteaStatePath`, `deploy.BlobsStatePath`, and `deploy.StateVersionPath` are this layout's single spelling. Deploy, backup, and restore all call them rather than rebuilding the paths independently — three copies of one layout decision is how it drifts.

## State export interfaces

Each of the four state kinds (spec.md "The four kinds of state") gets an export interface in `internal/core/state`, consumed by `backup/` and by the operator's own replication tooling.

| Kind | Interface | Shape |
|---|---|---|
| Git | `GitExporter.Remotes` | lists bare repositories under a Forgejo repo root, each as a `Remote{Name, URL}` ready for `git push --mirror` |
| Database | `DatabaseExporter.Snapshot` | streams a point-in-time copy via SQLite's online-backup API; the source is never paused |
| Blobs | `BlobExporter` | the read side of `blob.Adapter` |
| Key material | `KeyExporter.Names` / `.Resolve` | enumerates the fixed set of key names a bundle carries, then reads one from a `keystore.Driver` |

Two constraints shape the implementations:

- **Key names are a fixed list, not a query.** No shipped keystore driver can enumerate what it holds, so the set is declared rather than discovered. The age backup key is not in it — it encrypts the backup rather than travelling inside one, and the operator holds it directly.
- **Git is reached on the host; the database is reached through the container.** ORCH-001 guarantees only Docker and SSH on the host, so the database export runs `sqlite3` inside the pinned Forgejo image via `docker exec`. Git data is a host-visible directory (see Host state layout), so it is read directly.

## Driver interfaces

All three follow one posture: a Go interface for in-tree drivers, plus an exec-based protocol for out-of-tree ones. The exec protocol is generic and lives once, in `internal/core/driver` (CORE-003): `driver.Exec` runs an executable once per call, writing `{"method", "params"}` to its stdin and reading `{"ok", "result", "error"}` from its stdout. One process per call, no long-lived session.

- **DNS:** `Set(record, value, ttl)`, `Delete(record)`. Shipped: `cloudflare`, `rfc2136`.
- **Keystore:** `Resolve(keyName) → secret` on every driver; `Store(keyName, secret)` on `file` only. Shipped: `file`, `command`.
- **Blob:** `List`, `Get`, `Put`, streaming. Shipped: `local`, `s3`. Every `List` result carries `Modified`, the time an object was last written; an exec adapter written before that field existed omits it, which decodes as the zero time meaning *unknown* — never "very old".

ACME DNS-01 uses lego's own provider set and is independent of the DNS driver interface.

### The rotation guard

Key material is non-rotating by default (spec.md "Identity" > "Key material"). The guard enforcing that sits in `keystore`, **above** every driver rather than inside any of them, so an out-of-tree exec driver gets the same protection without implementing it. It consults a fixed registry: the TLS certificate and its private key are the only names permitted to overwrite.

The check is **fail-closed on the lookup itself**. A driver must return an error satisfying `errors.Is(err, keystore.ErrNotFound)` when it has positively determined a key is absent. Any other error — permission denied, I/O failure, an exec driver timing out or answering malformed — means the check failed, not that the key is missing, and the write is refused. This is a protocol requirement on every driver, in-tree or not.

### Keystore driver config

- **`file`** (`config.path`): a local directory, one file per key. `Resolve` reads `path/keyName` verbatim; `Store` writes it.
- **`command`** (`config.command`): one command run via `sh -c`, with `FARRIER_KEY_NAME` in its environment; returns trimmed stdout. One command branches on the env var to serve every key.
- **Anything else** resolves through the CORE-003 exec protocol: `config.path` is the executable, `config.args` its fixed arguments; method `resolve`, params `{"key": keyName}`, result `{"secret": "<base64>"}`.

## Orchestration

- Transport: SSH (Go `x/crypto/ssh`), authenticated by the operator's existing SSH agent (`SSH_AUTH_SOCK`) or an explicit key file — no other auth path, no password prompts.
- Host identity is checked against the operator's `known_hosts`; an unrecorded or mismatched host key fails the connection rather than prompting, since jobs run unattended.
- Host readiness beyond SSH is a single check: Docker reachable over the same session. Farrier requires nothing else of the host.
- The CLI renders Compose files from the manifest, ships them to the host, and drives `docker compose` over the SSH session.
- The stateless layer is disposable: `up` converges the host to the bundle definition idempotently and drift is overwritten. State under `<RemoteDir>/state` is not.

## Forge configuration

- Forgejo `app.ini` is fully rendered from the manifest — the install wizard is pre-answered by configuration.
- Admin bootstrap runs `forgejo admin user create` post-start; credentials are emitted once through the event stream.
- Rendered `app.ini` enables Actions. Forgejo's fork-PR approval gate is unconditional once Actions is on — it exposes no key to loosen it — so enabling Actions is the whole of that requirement.
- Drill mode adds four overrides to the rendered `app.ini` — webhooks off, webhook host allow-list empty, mailer off, mirrors off — and publishes the forge's HTTPS port on the deploy host's loopback interface rather than every interface. Quarantine is a render- and deploy-time override; the restored state itself is never edited.
- CI reconciliation at promote: a direct SQLite update resetting `running` → `queued` in the actions tables, before services start.

## API

- Loopback HTTP, default `127.0.0.1:7433`.
- RPC verbs: `POST /init`, `POST /up`, `POST /backup`, `POST /restore`, `POST /promote`, `POST /upgrade`, `POST /drill`, `POST /import`, `GET /status`, `GET /snapshots`.
- `GET /snapshots?to=<uri>` lists a destination's backup history — key, size, capture time, age — newest first. It reaches no host, so history stays readable while the forge is down.
- Mutation verbs return `{ jobId }`; `GET /jobs/{id}/events` streams progress over SSE.
- One event schema for all operations: `{ jobId, step, state, detail, timestamp }`. The CLI renders these in the terminal; the dashboard renders them in the browser.
- An operation that needs operator confirmation takes it as an explicit request field. The API cannot prompt, so the default must be refusal rather than silent execution.

## Operational targets

- **RTO:** promote completes — snapshot pulled, restored, verified, services live, DNS flipped — within 10 minutes on the reference instance (10 GB state, 100 Mbps between backup target and new host).
- **RPO:** equals backup cadence, which the operator sets — Farrier ships no scheduler. The golden-path cron is an example, not a default.
- **Backup impact:** push-hold is database-only and stays a second or two regardless of git data size; reads and fetches are uninterrupted throughout. A configurable ceiling bounds both the capture and the release that always follows it — a bug backstop, not a growth alarm.
- **Forge host floor:** 2 vCPU, 2 GB RAM, Linux x86_64 or arm64, Docker ≥ 24.
- **Control plane:** macOS and Linux; single static binary per platform.
- **DNS TTL:** 60 seconds on all bundle records.
- **Cert renewal:** lego renews at two-thirds of cert lifetime; `status` warns inside 14 days of expiry.
- **Drill cost:** a drill runs on any scratch Docker host, including the operator's own machine via local containers.

## Security posture

- API binds loopback; any wider exposure is operator topology.
- Key material is held in memory only during operations; never written to logs, event streams, command output, or the bundle directory.
- Snapshots are age-encrypted before leaving the forge host; the operator holds the sole key.
- CI executes in containers; the trust boundary and fork-PR policy are recorded in [spec.md](spec.md).
