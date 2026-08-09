# Spec

Design reference: every settled decision, what the system does, and how.

The system runs a complete self-hosted forge — git, pull requests, code review, secrets, CI/CD — as a portable bundle, operated through a CLI and a local web dashboard. The core idea: identity and state belong to the bundle, hosts are disposable, and moving the whole forge to a new host is a verified, one-command operation. The design splits cleanly into what the system owns (deployment, identity, verified backup and restore, promotion) and what the operator owns (hosts, replication topology, DNS redundancy).

## What it is

A tool that deploys, backs up, relocates, and restores a self-hosted forge, built as an orchestration and portability layer over existing open-source components. It ships as one binary with two frontends: a CLI and a local web dashboard.

In scope:

- Git hosting
- Pull requests and code review
- Secrets for CI
- CI/CD with runners
- Backup, replication interface, restore, and host-to-host migration of all of the above

## What it's built on

Every component is fully open source: Forgejo (GPL-3.0+), Forgejo Actions runner (MIT), SQLite (public domain), Caddy and Docker Compose (Apache 2). Components run as orchestrated containers, so their licenses place zero constraints on this project's own code.

- **Forge and CI:** Forgejo with Forgejo Actions (GitHub Actions-compatible syntax). One component covers git, pull requests, review, secrets, and CI orchestration — including Forgejo's complete web UI for daily use.
- **Runners:** Forgejo Actions runners. Colocated on the forge host by default; remote runners connect outbound to the bundle domain.
- **Database:** SQLite in WAL mode, managed by Forgejo. Forgejo's database holds metadata (users, pull requests, reviews, CI queue) while git data lives on disk, so the write load fits SQLite comfortably at the target scale: teams into the hundreds of users and thousands of repositories. SQLite also makes the database a single file with a clean online-backup API.
- **TLS termination:** Caddy, as a dumb terminator. The core owns ACME and hands Caddy its certificates.
- **Substrate:** Docker Compose on any Linux host with SSH. The Compose definitions are the bundle definition; the CLI wraps them.
- **Host:** the operator brings it. Requirements are Docker and SSH. The CLI targets `ssh://user@host`.

## Implementation

- **Language:** Go. One static binary with zero runtime dependencies carries the core engine, CLI, embedded web dashboard, SSH client, and Docker orchestration. Forgejo is also Go, keeping upstream source readable in one language.
- **ACME:** lego, in-process. Certificates are bundle key material, so the core issues and renews them; lego's DNS-01 support covers roughly one hundred DNS providers out of the box. That provider set is most of the binary — roughly 30 MB of the total, since it links every provider's SDK — and it is a deliberate trade. Certificate renewal runs unattended every sixty days, so it cannot degrade to manual the way a failover DNS flip can; bundling the set is what gives automatic renewal to operators whose provider Farrier ships no DNS driver for.
- **Backup encryption:** age.

## Stateless vs. stateful

Everything in the system is one of two things: disposable software or precious state. The split is what makes relocation fast.

- **Stateless:** forge app, CI orchestration, runners. Rebuilt from the bundle definition on any host in seconds.
- **Stateful:** repos, database, secrets, blobs, key material. Lives in one primary location.

The split is only real if the two live in different places. Containers are the disposable half and are recreated freely — every converge ships the whole Compose definition and lets `docker compose` reconcile — so no stateful kind may live inside one. State lives on the host filesystem, bind-mounted into the container that serves it, and survives a container being replaced or removed.

## The four kinds of state

State is decomposed into four kinds because each has a natural export and replication mechanism. A backup is complete when all four are present and mutually consistent, and the bundle manifest declares each kind with its checksums.

1. **Git data** — content-addressed. Exposed as a mirrorable set of git remotes; replicated via push mirrors.
2. **Database** — one SQLite file, exposed as a snapshot-consistent export via the online-backup API.
3. **Blobs** — LFS objects, CI artifacts, avatars. Accessed through a storage-adapter abstraction: a `local` adapter and an `s3` adapter ship with the system, and the adapter interface is published so operators can add their own.
4. **Key material** — carried in the bundle manifest (listed under Identity).

## Identity lives in the bundle, not the host

A forge's identity — its URL, its keys, its certificates — is what welds it to a machine. The bundle carries all of it, so relocation is a DNS flip instead of a re-pointing exercise.

### The domain

- Every bundle owns a DNS name the operator controls, required at `init`.
- All identity derives from it: clone URLs, webhooks, runner registration, OAuth callbacks, LFS endpoints. Hosts are fungible; the domain is permanent.
- `init` proves zone control via an ACME DNS-01 challenge, front-loading the project's one external dependency to day one.
- Records are created with a 60-second TTL so DNS flips land within the promotion downtime window.

### Key material

Generated at `init`, carried through every backup and restore:

- Forgejo `SECRET_KEY` and `INTERNAL_TOKEN`
- LFS JWT secret
- TLS certificates, issued and renewed by the core via ACME DNS-01 — a standby holds a valid cert before any traffic points at it
- SSH host keys, installed on every deploy including the first, so a fresh host and a restored one both present an unchanged identity

Key material is non-rotating by default: once `init` writes a piece of it, nothing may silently overwrite it, the same guarantee that keeps a second `init` from clobbering a live instance's identity. The TLS certificate and its private key are the one declared exception — an ACME-issued certificate is required to rotate before it expires. Every other piece above, plus the age backup key (spec.md "Key custody"), never rotates. A keystore driver's write side enforces this from a fixed rotation registry, consulted above the driver rather than trusted to it, so a piece of key material nobody has explicitly declared rotating defaults to protected.

### Runners across relocation

Runner registrations live in the database, and runners dial out to the domain. After promotion, remote runners reconnect automatically; colocated runners restart with the bundle.

## Who this is for: private repositories

The target is a team's private work — internal services, client projects, proprietary code — where the operator wants control over where the code lives and the ability to relocate it. Public open-source projects are better served by GitHub or Codeberg, whose value is the network: discovery, drive-by contributors, forks, and an identity contributors already have. Farrier replaces the parts of a forge a private team depends on, not the parts a public project depends on.

Public repositories work on a Farrier instance and are not restricted. They are simply not what the design optimizes for.

## CI trust boundary

The instance is single-tenant: CI exists to run the team's own code, isolated at the container level. The one place outside code can reach CI is a fork pull request on a public repository, so fork-PR workflows require maintainer approval before running — Forgejo enforces this unconditionally once Actions is enabled, which the bundle does by default. Private repositories, the target case, do not present this surface at all. Isolation against deliberately hostile code beyond container level is outside the design.

## Importing repositories

`import` wraps Forgejo's built-in migration to bring existing repositories in from GitHub or GitLab: code, full history, LFS objects, default branch, and optional mirror sync. Actions workflows arrive with the repo — they live in the repo tree and Forgejo Actions speaks GitHub Actions syntax, so CI largely ports as-is. CI secrets are re-entered by the operator on the new instance. Import covers repositories; issue and pull-request history stay on the source forge. Importing a batch reports each repository's own success or failure rather than one pass/fail for the whole run, and a repository whose migration fails is never left half-registered on the target.

## Version pinning

Every backup embeds the exact Forgejo version that wrote it, and restore always runs that exact version — the manifest pins the images, and restore uses the pinned images. Schema migrations run during upgrades, never during restores.

- **Restore:** boots the version recorded in the snapshot, every time.
- **Upgrade:** a deliberate command on a healthy instance — backup, bump the pinned version, run migrations, verify.

## What the operator owns

The system defines the state interface; the operator owns transport and topology.

- **Replication:** point any mechanism — object-storage sync, Litestream, rsync, git mirrors, multi-region fan-out — at the exported state interfaces. Topology, frequency, and consistency management are operator decisions.
- **DNS redundancy:** run any number of providers and failover schemes. The CLI updates configured providers through its driver system and prints the exact record change for everything else.
- **Golden path:** `backup --to <s3-uri-or-directory>` plus a one-line cron entry is the blessed minimum-effort route from `up` to redundancy.
- **Key custody:** backups are encrypted with age and the operator holds the key. A lost key means an unrecoverable backup. This is by design and stated prominently in the documentation.

## Bundle config is shareable; keys resolve through drivers

The bundle configuration (manifest, Compose definitions, pinned versions) is a plain directory designed to live in a private git repo or synced folder, so any teammate can operate the instance from their own machine. Key material stays out of the repo and resolves at runtime through a keystore driver:

- **`file`:** a path to a local directory; each piece of key material is a file named by its key name. The default.
- **`command`:** any command that prints the key — one interface that covers 1Password CLI, Vault, `pass`, sops, cloud secret managers, and anything else the team already uses.

The driver interface is published; the plugin posture matches DNS drivers and blob adapters. Teammate onboarding is: clone the bundle repo, obtain the key through the team's keystore.

The `file` driver's path — and the `local` blob adapter's, the same shape of config — must be absolute (XCUT-001). The manifest carries that path as the literal string an operator gave it; a relative one would silently re-resolve against whatever directory a command happens to run from, which differs by machine and even by shell session on the same machine. A teammate who wants `file` to work needs the key material reachable at the same absolute path everywhere they run Farrier from — a synced folder mounted consistently, not a path relative to wherever they happened to clone the bundle repo.

## What the system owns: verified restores

The CLI answers one question at restore and promote time: is this state a valid, complete, mutually consistent bundle?

- Manifest completeness: all four state kinds present, key material included.
- Checksums pass.
- Cross-consistency: database references to repos and blobs resolve to objects that exist.
- Any failure: refuse to proceed, report the specific missing or torn state.

## Backups

Backups are operator-initiated: Farrier ships no scheduler, and nothing runs one on the operator's behalf. The golden path's cron entry is a line the operator adds, not a default the system enables. A snapshot is the portability mechanism restore, promote, upgrade, and drill all consume — not a background redundancy job running on its own cadence.

The CLI produces coordinated, verified, encrypted snapshots of a live instance.

- **Database:** captured through SQLite's online-backup API — consistent with zero pause.
- **Git data:** `backup` holds incoming pushes only for the seconds the database capture takes, closing the window where a push could land between the database capture and the git data it references. Git objects are immutable and append-only, so tarring them can happen after the hold releases, with no consistency loss — only pinning each repository's ref state has to happen inside the hold, and that's a few kilobytes, not the repository itself. The hold is database-only and does not grow with git data. During the hold, a push is rejected outright with an explicit message, never queued or buffered — the client sees a clean failure and retries. Reads and fetches stay live throughout.
- **Encryption:** every snapshot is age-encrypted before it leaves the host.
- **Verification:** a backup that fails verification fails loudly at backup time.

## Failover

Failover is a manual, operator-initiated promotion with an accepted data-loss window and a few minutes of downtime. The operator decides; the CLI makes the decision informed and the execution one command.

- **Standby model: cold.** A standby is the latest verified snapshot in the backup target plus any fresh host. Promotion restores that snapshot onto the new host and flips DNS. Warm standbys (a second host kept continuously current) are operator topology built on the same interfaces.
- The CLI shows replication lag before promotion.
- Promotion runs four steps:
  1. Verify standby state currency; display lag.
  2. Reconcile CI: jobs marked `running` in the restored database are orphans — an artifact of capturing state mid-flight — so the reset to `queued` runs against the snapshot's database before it is placed on the host, ahead of the next step; they and all queued jobs re-dispatch to runners automatically, each in a fresh workspace, once services start.
  3. Start stateless services on the standby against the restored state.
  4. Flip DNS via a configured driver, or print the exact record change.
- DNS is the failover path's single external dependency. An outage at the operator's DNS provider sits outside the system's coverage and is documented as such.

## DNS drivers

DNS updates go through a driver plugin system: a published driver interface, two drivers shipped with the system, and community drivers beyond that.

- **Shipped:** Cloudflare API, RFC 2136 (nsupdate).
- **Everything else:** the CLI prints the exact record change for manual application.
- **Certificate renewal** is independent of this driver system: lego's DNS-01 support gives cert issuance and renewal coverage across roughly one hundred providers.

## Replication lag

`status` reports lag for golden-path transports the system configured. Operator-assembled topologies run outside the system's measurement and carry their own consistency responsibility.

## Interfaces: one core, thin frontends

All logic lives in a single core engine; the CLI and the web dashboard are thin clients of it. The control plane runs on the operator's machine, outside the failure domain, so every operation — including promotion — works when the forge host is down.

- **Core engine:** a library that owns everything real — manifest, state export, verification, backup, restore, promotion, ACME, DNS. Frontends contain zero logic.
- **CLI:** the same binary, calling the core directly as a library.
- **Web dashboard:** `ui` starts a localhost-only server on the operator's machine and opens the browser. It covers the portability layer: instance status, replication lag, backup history, drill results, and promotion. Day-to-day forge use (repos, pull requests, review, CI logs) happens in Forgejo's own web UI.
- **API:** RPC-style JSON over localhost HTTP — verb endpoints such as `POST /promote`, `POST /backup`, `GET /status`. It serves the dashboard and binds to loopback by default; exposing it beyond loopback (VPN, tailnet) is the operator's topology and the operator's business.
- **Long-running operations:** verbs return a job ID and progress streams over SSE. The core emits one event/progress model; the dashboard renders it in the browser and the CLI renders the same events in the terminal, so both frontends share a single code path for every operation.

## CLI commands

The full lifecycle — create, deploy, import, protect, relocate, verify, upgrade — in ten commands.

| Command | Function |
|---|---|
| `init` | Create a bundle: domain, zone-control proof, key material, manifest. |
| `import` | Bring existing repositories in from GitHub or GitLab, with history, LFS, and optional mirror sync. |
| `up` | Deploy the stateless layer against bundle state on a target host. Ends with the forge live in a browser: configuration fully rendered from the manifest, install wizard pre-answered, first admin account provisioned with credentials handed to the operator. |
| `backup` | Produce a complete, verified, encrypted snapshot to an S3 URI or directory. |
| `restore` | Rebuild a full instance from a snapshot onto a fresh host, verified, running the snapshot's pinned version. |
| `promote` | Fail over: restore the latest snapshot onto a fresh host, verify, start, reconcile CI, flip DNS. |
| `upgrade` | Backup, bump the pinned Forgejo version, run migrations, verify. |
| `drill` | Restore the latest backup to a quarantined scratch target, boot it, run a smoke CI job, report. |
| `status` | Instance health and replication lag for golden-path transports. |
| `ui` | Serve the local web dashboard on loopback and open the browser. |

## Rehearsal

`drill` keeps the escape hatch verified instead of trusted. It restores the most recent backup onto a scratch target (remote host or local container), boots the full stack in quarantine, runs a smoke CI job, and reports success or the specific failure.

Quarantine exists because a drill instance carries production's identity. In drill mode: outbound notifications (webhooks, email) are disabled by config override, DNS stays untouched, and the operator reaches the instance through an SSH tunnel. The drill proves the backup restores and CI runs while the outside world hears nothing.

## Distribution and licensing

Open-source repository. Operators clone it and run it on their own hardware. The project ships software and documentation; hosting, operation, and support of deployed instances belong to the operator.

Farrier is licensed Apache 2.0: permissive enough that anyone can use, fork, or commercialize it, while requiring that the license and NOTICE travel with derivative works and that modifications are stated — attribution back to the project without restricting use. Apache 2.0 also grants patent rights explicitly, which MIT and BSD leave unaddressed. Components run as orchestrated containers rather than linked code, so Forgejo's GPL-3.0-or-later places no obligation on this project's own source.
