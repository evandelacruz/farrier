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
  attach/             `attach` sequencing: names a nameless instance in place
  status/             instance health, last-backup age, replication lag
  importer/           `import` sequencing over Forgejo's migration API
  publish/            `publish` sequencing: the project folder's first push
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

A plain directory at `.farrier/` in the project it serves, versioned with it.

```
farrier.yaml          manifest: domain, published web port and public web
                      port, git-over-SSH host port, SSH host public key,
                      pinned image digests, driver config, ACME DNS-01
                      config, CI runner config, state-kind declarations,
                      checksum algorithm
compose/              rendered Docker Compose definitions
```

- Manifest format: YAML.
- Versions are pinned by image digest, not tag.
- `domain` is optional, and absent is what makes a bundle nameless (INIT-005, spec.md "Instances without a name"). A nameless manifest carries no `acme` section either, and the two must agree: a named bundle states the DNS-01 provider its zone was proven through, a nameless one states nothing.
- `acme.directoryUrl` is the ACME server the bundle's certificates are issued and renewed against. `init` resolves the operator's choice — nothing, the shorthand `staging`, or a URL — to an absolute directory URL and writes that, so the manifest carries no shorthand to interpret. Absent is a manifest written before the field existed and means Let's Encrypt production. Every path that issues or renews reads it: a certificate issued by one CA must not be renewed by another (spec.md "The domain").
- `webPort` is the host port `up` publishes Caddy on. Absent takes the tier's default: 443 for a named bundle, 8222 for a nameless one. Only the host side of the mapping moves; Caddy's container port is fixed.
- `publicWebPort` is the port clients connect on when something already on the host holds the standard port and forwards to Farrier. Absent means Caddy is the edge, and the public URL uses `webPort`. A named bundle whose `webPort` is not 443 must set it — see spec.md "Reaching the forge" for why, and for the constraint that any such forwarder passes TCP through rather than terminating TLS.
- `actions.colocatedRunner` is the CI runner config: `false` keeps the bundled Actions runner off the forge host, and the operator registers a remote runner against the bundle domain instead (spec.md "CI trust boundary"). Absent means enabled.
- `sshHostKeyPublic` is the instance's SSH host key, public half, in OpenSSH authorized-keys format — the fingerprint `publish` renders into a `known_hosts` entry so a host answering with a different key fails the push. `init` writes it from the keystore, which keeps the private half and stays the source of truth. Absent is a manifest written before the field existed; readers fall back to the keystore rather than skipping the pin.
- Key material is referenced by keystore driver config, never stored — the host key's public half above is the exception, and it is public by definition.
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
<RemoteDir>/state/gitea/conf/app.ini
                           the file the rendered `<RemoteDir>/forge/app.ini`
                           mounts onto; it falls inside the mount above, so
                           `up` creates it — empty, and only when absent —
                           rather than leaving a container runtime to create
                           it under a host mount, which not all of them can
<RemoteDir>/state/blobs    created, not mounted — reserved for a `local` blob
                           adapter; no container reads it today
<RemoteDir>/state/forgejo-version
                           the Forgejo image this state has been started
                           under; one line, not mounted
```

These directories have to be usable by the uid the Forgejo container runs as, and how they get that way is not the same on every host: a Linux bind mount passes real uids through, so ownership must actually be set, while a macOS container runtime maps ownership across the mount boundary, where setting it is both unnecessary and refused to any user who is not root. So `up` sets ownership best-effort and then verifies the outcome rather than the mechanism — it runs the pinned Forgejo image as that uid, over these same mounts, and reads and writes a probe file in each directory, plus the SSH host key under `state/gitea`, which is shipped `0600` and useless if the container cannot read it. Ownership that could not be set is reported and not fatal; a forge that cannot use its state fails `up` at that step, naming the directory and the fix. Nothing detects the host's operating system or whether the target is local — that is the locality-dependent behavior ORCH-003 exists to rule out.

Each command verifies the state it placed, over the paths it placed. `up` creates the two directories above and checks those. `restore` writes a directory per repository under `state/git` and the database under `state/gitea` before `up` runs at all, and checks those, the same way and in the same image. Neither stands in for the other: on a target whose state directories already exist and are already forge-owned — a restore re-run, an unfinished drill teardown, recovery onto a provisioned host — a top-level check passes while freshly restored content underneath is unusable, and a restore that cannot be used is a restore that failed.

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
- **Keystore:** `Resolve(keyName) → secret` on every driver; `Store(keyName, secret)` on the optional `Writer` side, which `init` requires; `DescribeTarget(keyName) → string` on the optional `Describer` side, which names where key material lands so `init` can report it (INIT-006). A driver that cannot say — `command` hands storage to an operator's command, and the exec protocol has no `describe` method — returns nothing and is reported by driver name alone. Shipped: `file`, `command`.
- **Blob:** `List`, `Get`, `Put`, streaming. Shipped: `local`, `s3`. Every `List` result carries `Modified`, the time an object was last written; an exec adapter written before that field existed omits it, which decodes as the zero time meaning *unknown* — never "very old".

ACME DNS-01 uses lego's own provider set and is independent of the DNS driver interface. The ACME server itself is a manifest field rather than a driver: the protocol is one standard, so reaching Let's Encrypt staging, an internal CA, or any other issuer is a directory URL, not a second implementation.

### The rotation guard

Key material is non-rotating by default (spec.md "Identity" > "Key material"). The guard enforcing that sits in `keystore`, **above** every driver rather than inside any of them, so an out-of-tree exec driver gets the same protection without implementing it. It consults a fixed registry: the TLS certificate and its private key are the only names permitted to overwrite.

The check is **fail-closed on the lookup itself**. A driver must return an error satisfying `errors.Is(err, keystore.ErrNotFound)` when it has positively determined a key is absent. Any other error — permission denied, I/O failure, an exec driver timing out or answering malformed — means the check failed, not that the key is missing, and the write is refused. This is a protocol requirement on every driver, in-tree or not.

### Keystore driver config

- **`file`** (`config.path`): a local directory, one file per key. `Resolve` reads `path/keyName` verbatim; `Store` writes it.
- **`command`** (`config.command`, `config.storeCommand`): each is one command run via `sh -c` with `FARRIER_KEY_NAME` in its environment. `config.command` returns the secret as trimmed stdout; `config.storeCommand` receives it on stdin and exits zero on success. One command of each kind branches on the env var to serve every key. A driver configured without `storeCommand` is resolve-only, and `init` fails clearly against it rather than minting somewhere else. A resolve command that exits **zero with no output** is this driver's positive "not found" — the answer the rotation guard requires before it will write. A non-zero exit is a failure, never an absence.
- **Anything else** goes through the CORE-003 exec protocol: `config.path` is the executable, `config.args` its fixed arguments. Method `resolve`, params `{"key": keyName}`, result `{"secret": "<base64>", "found": true|false}`; method `store`, params `{"key": keyName, "secret": "<base64>"}`, empty result. `config.store: true` declares the executable implements `store`; absent means resolve-only. **Absence is stated, not inferred from an empty secret.** `found: false` is this protocol's positive "not found" — the same answer `command` gives by exiting zero with no output, and the one the rotation guard requires before it will write. `found: true` with `secret` empty or omitted is a malformed response, not an absence: a driver claiming to have found something and returning nothing has failed the check, so the guard refuses the write. A response omitting `found` is malformed for the same reason — it states nothing. A failed call is never an absence.

  **Store capability is a construction-time fact for every driver, and that is load-bearing.** `init` type-asserts `keystore.Writer` at validate, before proving zone control or generating anything, so an operator pointed at a keystore that cannot accept key material is told immediately rather than after an ACME round trip. `file` always writes and `command` decides on the presence of `storeCommand` — both settled before the driver exists. An exec driver cannot decide from the executable, since one Go type serves every one of them, so it decides from `config.store` instead. Declaring `store: true` against an executable that does not implement it fails at the first `store` call, which is the operator's own misconfiguration rather than a hole in the guarantee.

## Orchestration

- Transport: SSH (Go `x/crypto/ssh`), authenticated by the operator's existing SSH agent (`SSH_AUTH_SOCK`) or an explicit key file — no other auth path, no password prompts.
- Host identity is checked against the operator's `known_hosts`; an unrecorded or mismatched host key fails the connection rather than prompting, since jobs run unattended.
- Host readiness beyond SSH is a single check: Docker reachable over the same session. Farrier requires nothing else of the host.
- The CLI renders Compose files from the manifest, ships them to the host, and drives `docker compose` over the SSH session.
- An SSH command session is non-interactive and non-login, so a `docker` that a shell rc file puts on PATH is invisible to it. Every command the transport sends carries a preamble that appends a fixed candidate list — `$HOME/.docker/bin`, `/usr/local/bin`, `/opt/homebrew/bin`, `/snap/bin` — to PATH, and only when `docker` does not already resolve. Nothing on the host has to be installed, configured, or exported for this to work.
- The stateless layer is disposable: `up` converges the host to the bundle definition idempotently and drift is overwritten. State under `<RemoteDir>/state` is not.

## Forge configuration

- Forgejo `app.ini` is fully rendered from the manifest — the install wizard is pre-answered by configuration.
- Admin bootstrap runs `forgejo admin user create` post-start; credentials are emitted once through the event stream.
- Rendered `app.ini` enables Actions. Forgejo's fork-PR approval gate is unconditional once Actions is on — it exposes no key to loosen it — so enabling Actions is the whole of that requirement.
- Drill mode adds four overrides to the rendered `app.ini` — webhooks off, webhook host allow-list empty, mailer off, mirrors off — publishes the forge's HTTPS port on the deploy host's loopback interface rather than every interface, and gives Caddy the bundle domain as a Compose network alias so the drilled host resolves that domain to the drilled instance rather than to production. Quarantine is a render- and deploy-time override; the restored state itself is never edited, and no DNS record is touched.
- CI reconciliation at promote: a direct SQLite update resetting `running` → `queued` in the actions tables, before services start.

## API

- Loopback HTTP, default `127.0.0.1:7433`.
- RPC verbs: `POST /init`, `POST /up`, `POST /attach`, `POST /backup`, `POST /restore`, `POST /promote`, `POST /upgrade`, `POST /drill`, `POST /import`, `POST /publish`, `GET /status`, `GET /snapshots`.
- `GET /snapshots?to=<uri>` lists a destination's backup history — key, size, capture time, age — newest first. It reaches no host, so history stays readable while the forge is down.
- Mutation verbs return `{ jobId }`; `GET /jobs/{id}/events` streams progress over SSE.
- One event schema for all operations: `{ jobId, step, state, detail, timestamp }`. The CLI renders these in the terminal; the dashboard renders them in the browser.
- An operation that needs operator confirmation takes it as an explicit request field. The API cannot prompt, so the default must be refusal rather than silent execution.

## Operational targets

- **RTO:** promote completes — snapshot pulled, restored, verified, services live, DNS flipped — within 10 minutes on the reference instance (10 GB state, 100 Mbps between backup target and new host).
- **RPO:** equals backup cadence, which the operator sets — Farrier ships no scheduler. The golden-path cron is an example, not a default.
- **Backup impact:** push-hold is database-only and stays a second or two regardless of git data size; reads and fetches are uninterrupted throughout. A configurable ceiling bounds both the capture and the release that always follows it — a bug backstop, not a growth alarm.
- **First deployment:** `up` gives Forgejo three minutes to finish the database migrations its first boot runs, and any other container one minute to start. Only a genuinely first boot spends that budget — a re-run, a restore, and a promote all open a schema that already exists — so the RTO above is unaffected.
- **Forge host floor:** 2 vCPU, 2 GB RAM, Linux x86_64 or arm64, Docker ≥ 24.
- **Control plane:** macOS and Linux; single static binary per platform.
- **DNS TTL:** 60 seconds on all bundle records.
- **Cert renewal:** lego renews at two-thirds of cert lifetime; `status` warns inside 14 days of expiry.
- **Drill cost:** a drill runs on any scratch Docker host, including the operator's own machine via local containers.

## Security posture

- API binds loopback; any wider exposure is operator topology.
- Secret key material is held in memory only during operations; never written to logs, event streams, command output, or the bundle directory. Public key material, such as the SSH host key's public half, may live in the manifest: it is the string an operator pastes into `known_hosts`, and requiring keystore access to obtain it would mean handing out the store that holds `SECRET_KEY` and the age backup key just to let someone publish a repository.
- Snapshots are age-encrypted before leaving the forge host; the operator holds the sole key.
- CI executes in containers; the trust boundary and fork-PR policy are recorded in [spec.md](spec.md).
