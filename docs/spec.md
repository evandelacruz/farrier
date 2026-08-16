# Spec

Design reference: every settled decision, what the system does, and how.

The system turns a project folder into a complete self-hosted forge — git, pull requests, code review, secrets, CI/CD — with a remote ready to push to, operated through a CLI and a local web dashboard. The core idea: identity and state belong to the bundle, hosts are disposable, and moving the whole forge to a new host is a verified, one-command operation. The design splits cleanly into what the system owns (deployment, identity, verified backup and restore, promotion) and what the operator owns (hosts, replication topology, DNS redundancy).

## What it is

Given any project folder, Farrier stands up a self-contained self-hosted forge for it — git hosting, pull requests, code review, and CI/CD — with a remote ready to push to. Each project gets its own portable forge instance rather than a shared central server.

It ships as one binary with two frontends: a CLI and a local web dashboard, built as an orchestration and portability layer over existing open-source components.

In scope:

- Git hosting
- Pull requests and code review
- Secrets for CI
- CI/CD with runners
- Backup, replication interface, restore, and host-to-host migration of all of the above

## What it's built on

Every component is fully open source: Forgejo (GPL-3.0+), Forgejo Actions runner (MIT), SQLite (public domain), Caddy and Docker Compose (Apache 2). Components run as orchestrated containers, so their licenses place zero constraints on this project's own code.

- **Forge and CI:** Forgejo with Forgejo Actions (GitHub Actions-compatible syntax). One component covers git, pull requests, review, secrets, and CI orchestration — including Forgejo's complete web UI for daily use.
- **Runners:** Forgejo Actions runners. The bundle deploys one colocated on the forge host by default, so a fresh `up` gives working CI; remote runners connect outbound to the bundle domain. Actions runs each job in a container, so the runner needs the host's Docker socket to start them — see "CI trust boundary" for the trade that carries, and for the remote-runner escape hatch.
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

- A named bundle owns a DNS name the operator controls, given at `init`. A bundle may also be nameless ("Instances without a name"), which trades permanence for a first minute that costs nothing.
- All identity derives from the name: clone URLs, webhooks, runner registration, OAuth callbacks, LFS endpoints. Hosts are fungible; the domain is permanent. A nameless instance puts its address in that role, which is why relocating one breaks remotes.
- Given a name, `init` proves zone control via an ACME DNS-01 challenge, front-loading the project's one external dependency to day one.
- Records are created with a 60-second TTL so DNS flips land within the promotion downtime window.

### Reaching the forge

Two ports carry every client protocol, and both are part of the identity the bundle owns:

- **HTTPS on 443 by default**, terminated by Caddy with a core-issued certificate. Browser, REST API, git-over-HTTPS, and LFS all arrive here.
- **Git over SSH on 2222 by default**, served by Forgejo's own SSH server using the bundle's host key. The port is a manifest field, so an operator whose host sshd lives elsewhere can set 22 and get bare `git@domain:owner/repo.git` URLs. The default is 2222 because the host's sshd normally owns 22 — `up` reaches hosts over it (ORCH-001), and taking it would require reconfiguring the host, which Farrier deliberately does not ask for. The cost of the default is a port in the SSH clone URL, which Forgejo renders into the URLs it displays.

Both ports are manifest fields, because the operator brings the host and Farrier is not entitled to assume it owns anything on it. A nameless instance's web port defaults to 8222 rather than 80: the nameless tier exists so Farrier can be tried on the machine the operator is sitting at, and 80 is the port that machine is most likely to have taken already.

**Where Caddy is published and what clients are told are two facts, not one.** The published port is where Caddy listens on the host. The public URL — `ROOT_URL`, every clone URL, runner registration — is what clients connect to, and is derived from the domain (or the nameless instance's address) plus a port, omitting the port when the scheme already implies it. They are the same number whenever Farrier's Caddy is the edge, which is the ordinary case. They differ only when something already on the host holds the standard port and forwards, and then the operator states the public port explicitly. Farrier cannot see a forwarder, and a named instance published off 443 that says nothing is refused rather than deployed: guessing wrong brings up a healthy forge whose every rendered link is unreachable.

**A forwarder must pass TCP through and let Caddy terminate.** Farrier owns the certificate — identity lives in the bundle, and restore and promote deliver an unchanged TLS identity on new hardware. A proxy that terminates TLS itself hands clients a certificate that is not the bundle's, which breaks that guarantee. SNI routing to Farrier's Caddy is supported; upstream termination is a different product, not a configuration option.

The ports belong to the instance: one bundle owns one domain, and every repository on it answers at the same endpoint.

The host key is bundle key material, so a restored or promoted instance answers on the same port with the same key and existing remotes and `known_hosts` entries keep working.

### Key material

Generated at `init`, carried through every backup and restore:

- Forgejo `SECRET_KEY` and `INTERNAL_TOKEN`
- LFS JWT secret
- TLS certificates, issued and renewed by the core via ACME DNS-01 — a standby holds a valid cert before any traffic points at it
- SSH host keys, installed on every deploy including the first, so a fresh host and a restored one both present an unchanged identity

Secrets stay in the keystore; the SSH host key's public half also travels in the bundle manifest, written there at `init`. That half is public by definition — it is the string an operator would paste into `known_hosts` — and carrying it means publishing a project to an instance can pin its identity without read access to the store holding `SECRET_KEY`, `INTERNAL_TOKEN`, and the age backup key. On a shared instance, that is the difference between every project owner being able to publish and only the instance's owner. The private half never leaves the keystore, and nothing secret is ever written into a bundle.

Key material is non-rotating by default: once `init` writes a piece of it, nothing may silently overwrite it, the same guarantee that keeps a second `init` from clobbering a live instance's identity. The TLS certificate and its private key are the one declared exception — an ACME-issued certificate is required to rotate before it expires. Every other piece above, plus the age backup key (spec.md "Key custody"), never rotates. A keystore driver's write side enforces this from a fixed rotation registry, consulted above the driver rather than trusted to it, so a piece of key material nobody has explicitly declared rotating defaults to protected.

### Runners across relocation

Runner registrations live in the database, and runners dial out to the domain. After promotion, remote runners reconnect automatically; colocated runners restart with the bundle.

## The unit: one forge per project

The thing Farrier hands you is a forge for one project, and the project folder is where it starts.

- **The bundle lives in the project, at `.farrier/`.** Manifest and rendered Compose definitions sit beside the code, holding no secrets, so the forge definition is versioned with the thing it serves and travels with it. This is the default and the shape the design optimizes for: `cd my-project && farrier init`, and that project has its own forge.
- **One instance may serve several projects, and then the bundle lives on its own.** An instance hosts as many repositories as the operator puts on it — ten projects on one instance is one address, one backup, one drill, and a Forgejo that lists all ten. Nothing in the code differs; it is how many times `init` and `up` are run. But a bundle serving ten projects belongs to none of them, so `init` takes an explicit location for that case rather than making one project arbitrarily own the forge that hosts the other nine. A location argument, not a mode.
- **`init` takes a folder; `up` takes a host.** They stay separate commands. `init` makes a folder into a forge definition; `up` puts it on a machine.
- **A host is a host.** `ssh://user@localhost` and `ssh://user@a-vps` run the same path. There is no local mode — locality is an argument, not a branch. ACME DNS-01 proves zone control by writing a TXT record rather than answering an inbound request, so an instance on the operator's own machine holds a publicly valid certificate for its name exactly like a remote one.
- **An FQDN belongs to an instance, not a repository.** Apex or subdomain is immaterial — what matters is a name the operator controls in DNS, unique to that instance, since the name is the identity. Every repository on that instance shares the endpoint. Subdomains are simply the cheap way to run many instances: one owned zone, a name per project, nothing new to register.
- **Shared or not is usage, not a mode.** An instance hosts as many repositories as the operator puts on it. One project per instance is the default shape and the reason the design carries no org, team, or tenancy modeling — but nothing forbids several, and no code path differs.

The first push is part of standing it up: `publish` creates the repository on the instance from the folder, pushes its existing history, and sets `origin` to the instance's SSH URL. It refuses rather than overwrites — a folder with no commits, a folder that already has that remote, and a repository already on the instance each fail with nothing changed. `import` (below) remains the on-ramp for a project that already lives on GitHub or GitLab.

A fresh instance has no SSH key on the account, and a push to an account with no key is rejected — so `publish` registers the operator's own public key when the account has none, rather than failing the one command the quick start says takes no flags. It is a fallback, not a policy: an account that already has a key is already publishable and is left untouched, the operator can name a different key, and the file that was registered is reported in the run's output, because uploading a key to their account is a change they are entitled to see.

### Instances without a name

Requiring a domain before anything works puts the cost first: own a name, hold a DNS API token or paste a TXT record and wait for it to propagate, and only then get a forge. For someone trying the thing for the first time, that is where they stop.

So a name is optional. `init` with no domain produces a **nameless bundle** — no DNS-01 proof, no certificate, nothing for the operator to own — and `up` serves it over plain HTTP at whatever address the operator gives, on their own machine or on a remote host. Every other piece of key material is generated as usual: a nameless instance is a complete instance in all respects but its name.

What that costs, stated plainly:

- **The web UI is unencrypted.** Git over SSH is encrypted regardless — pushing to a nameless instance across the internet is safe — but pull requests, review, and login travel in the clear, so a nameless instance belongs on a LAN, a VPN, or a tailnet.
- **The address is the identity, so moving breaks remotes.** A named instance relocates by DNS flip and no remote ever changes; that is what identity-in-the-bundle buys, and a nameless instance is the one case that opts out. Attaching a domain later is supported and in-place — repositories, history, pull requests, review comments, CI history, secrets, and the SSH host key all survive, because they are bundle and host state rather than identity. Consumers re-point their remote once.

Every command that addresses the instance takes the address in the domain's place, `publish` included: it points `origin` at that address and pins the push to the host key there, taking the address from the operator or, by default, from the host of the API URL they already named. The two can differ when the API is reached through a tunnel or a proxy, which is why the address stays statable on its own.

A tailnet name is the best of the nameless options: stable, private, reachable from anywhere the operator is logged in, and it does not drift the way an IP does.

Named from the start remains the recommendation for anything that will outlive the experiment. The nameless tier exists so the first minute costs nothing, not so the domain can be avoided forever.

## Who this is for: private repositories

The target is a team's private work — internal services, client projects, proprietary code — where the operator wants control over where the code lives and the ability to relocate it. Public open-source projects are better served by GitHub or Codeberg, whose value is the network: discovery, drive-by contributors, forks, and an identity contributors already have. Farrier replaces the parts of a forge a private team depends on, not the parts a public project depends on.

Public repositories work on a Farrier instance and are not restricted. They are simply not what the design optimizes for.

## CI trust boundary

The instance is single-tenant: CI exists to run the team's own code, isolated at the container level. The one place outside code can reach CI is a fork pull request on a public repository, so fork-PR workflows require maintainer approval before running — Forgejo enforces this unconditionally once Actions is enabled, which the bundle does by default. Private repositories, the target case, do not present this surface at all. Isolation against deliberately hostile code beyond container level is outside the design.

### The colocated runner holds the host's Docker socket

Actions runs every job step in a container the workflow names, so the runner must be able to create containers — and a container cannot do that alone. The colocated runner is therefore given the host's Docker socket. Anything that can reach that socket can start any container, including one mounting the whole host filesystem, so **a workflow that runs on the default deployment can take the forge host** — the same host holding git data and the database.

This is stated plainly because "isolated at the container level" reads stronger than what ships. It is a deliberate trade, on these grounds:

- It is what self-hosted Actions normally does, so it is the least surprising thing for an operator to find.
- The alternatives do not buy isolation cheaply. Privileged Docker-in-Docker is equally root on the host. Rootless Docker-in-Docker is genuinely isolated but needs specific storage-driver and kernel support, which contradicts running on any Linux host with Docker and SSH.
- The realistic threat is not a teammate writing hostile code; it is a compromised build dependency executing during a job. That risk exists in every CI system. What Farrier changes is the blast radius, since the runner sits on the host that holds the state.

**The escape hatch is topology, not configuration.** An operator who does not want that risk on the forge host disables the colocated runner and registers a remote one against the bundle domain. The remote runner's own host still holds a Docker socket — the risk moves rather than disappearing — but it moves onto a disposable machine that carries no forge state. Nothing else about the instance changes: runner registrations live in the database and survive backup, restore, and promotion either way.

## Importing repositories

`import` wraps Forgejo's built-in migration to bring existing repositories in from GitHub or GitLab: code, full history, LFS objects, default branch, and optional mirror sync. Actions workflows arrive with the repo — they live in the repo tree and Forgejo Actions speaks GitHub Actions syntax, so CI largely ports as-is. CI secrets are re-entered by the operator on the new instance. Import covers repositories; issue and pull-request history stay on the source forge. Importing a batch reports each repository's own success or failure rather than one pass/fail for the whole run, and a repository whose migration fails is never left half-registered on the target.

## Version pinning

Every backup embeds the exact Forgejo version that wrote it, and restore always runs that exact version — the manifest pins the images, and restore uses the pinned images. Schema migrations run during upgrades, never during restores.

- **Init:** resolves whatever image reference it is given — a tag, or a digest — to a digest, and writes that digest into the manifest. From that moment the bundle is frozen there.
- **Up:** deploys the digest the manifest holds. It never re-resolves, so a bundle deployed today and the same bundle deployed in six months are the same software. A tag in the default image set therefore governs exactly one thing: what a **fresh** `init` picks up. It does not mean a deployed instance drifts forward onto patches.
- **Restore:** boots the version recorded in the snapshot, every time.
- **Upgrade:** the only command that moves a bundle to a different version, and a deliberate one on a healthy instance — backup, bump the pinned version, run migrations, verify.

## What the operator owns

The system defines the state interface; the operator owns transport and topology.

- **Replication:** point any mechanism — object-storage sync, Litestream, rsync, git mirrors, multi-region fan-out — at the exported state interfaces. Topology, frequency, and consistency management are operator decisions.
- **DNS redundancy:** run any number of providers and failover schemes. The CLI updates configured providers through its driver system and prints the exact record change for everything else.
- **Golden path:** `backup --to <s3-uri-or-directory>` plus a one-line cron entry is the blessed minimum-effort route from `up` to redundancy.
- **Key custody:** backups are encrypted with age and the operator holds the key. A lost key means an unrecoverable backup. This is by design and stated prominently in the documentation.

## Bundle config is shareable; keys resolve through drivers

The bundle configuration (manifest, Compose definitions, pinned versions) is a plain directory living at `.farrier/` in the project it serves, so any teammate who can clone the project can operate the instance from their own machine. Secrets stay out of the repo and resolve at runtime through a keystore driver:

- **`file`:** a path to a local directory; each piece of key material is a file named by its key name. The default.
- **`command`:** any command that prints the key, plus a second that takes one on stdin — one interface that covers 1Password CLI, Vault, `pass`, sops, cloud secret managers, and anything else the team already uses. Both halves matter: `init` mints key material and has to put it somewhere, and a driver that can only read forces the operator to mint into a file first and copy it across, leaving a plaintext copy on disk in exactly the place they were trying to avoid.

The driver interface is published; the plugin posture matches DNS drivers and blob adapters. Teammate onboarding is: clone the project, obtain the key through the team's keystore.

The `file` driver's path — and the `local` blob adapter's, the same shape of config — must be absolute (XCUT-001). The manifest carries that path as the literal string an operator gave it; a relative one would silently re-resolve against whatever directory a command happens to run from, which differs by machine and even by shell session on the same machine. A teammate who wants `file` to work needs the key material reachable at the same absolute path everywhere they run Farrier from — a synced folder mounted consistently, not a path relative to wherever they happened to clone the project.

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

The full lifecycle — create, deploy, publish, import, name, protect, relocate, verify, upgrade — in twelve commands.

| Command | Function |
|---|---|
| `init` | Make a project folder into a forge definition: domain, zone-control proof, key material, manifest, written to `.farrier/`. |
| `publish` | Put the project folder on its instance: create the repository, push the folder's existing history, set `origin` to the instance's SSH URL. |
| `import` | Bring existing repositories in from GitHub or GitLab, with history, LFS, and optional mirror sync. |
| `up` | Deploy the stateless layer against bundle state on a target host. Ends with the forge live in a browser: configuration fully rendered from the manifest, install wizard pre-answered, first admin account provisioned with credentials handed to the operator. |
| `backup` | Produce a complete, verified, encrypted snapshot to an S3 URI or directory. |
| `restore` | Rebuild a full instance from a snapshot onto a fresh host, verified, running the snapshot's pinned version. |
| `promote` | Fail over: restore the latest snapshot onto a fresh host, verify, start, reconcile CI, flip DNS. |
| `attach` | Give a nameless instance a domain in place: prove the zone, issue TLS, re-render, report the clone URLs that changed. |
| `upgrade` | Backup, bump the pinned Forgejo version, run migrations, verify. |
| `drill` | Restore the latest backup to a quarantined scratch target, boot it, run a smoke CI job, report. |
| `status` | Instance health and replication lag for golden-path transports. |
| `ui` | Serve the local web dashboard on loopback and open the browser. |

## Rehearsal

`drill` keeps the escape hatch verified instead of trusted. It restores the most recent backup onto a scratch target (remote host or local container), boots the full stack in quarantine, runs a smoke CI job, and reports success or the specific failure.

Quarantine exists because a drill instance carries production's identity. In drill mode: outbound notifications (webhooks, email) are disabled by config override, DNS stays untouched, and the operator reaches the instance through an SSH tunnel. The drill proves the backup restores and CI runs while the outside world hears nothing.

## Farrier hosts a real project

A private project moves its development onto an instance Farrier deploys. This is the acceptance test for the whole system: every command runs against real work, on a repository whose loss would matter, with real pull-request and CI traffic, operated by the person who wrote the tool.

The subject is a private project rather than this repository. Farrier is built for private repositories ("Scope" in the README), so the acceptance test runs against the user profile the project claims.

The cutover, in order:

1. `init` a bundle for the project's domain, then `up` it on a host.
2. `import` the project from its current host — code, full history, LFS objects, default branch.
3. Re-enter CI secrets and re-create branch protection on the new instance. Neither travels with an import, and neither is Farrier's to carry: they are Forgejo configuration the operator owns.
4. Land one pull request end to end — branch, push over SSH, review, green CI on a Forgejo Actions runner, merge.
5. `backup`, then `drill`, before the old copy stops being the working one.

**Milestone met** when step 4 lands and step 5 passes. Pull requests, review comments, and CI history stay on the old host; the repository moves, its metadata does not.

### A copy of the bundle must survive the instance

The bundle lives at `.farrier/` in the project, and the project is hosted on the forge that bundle deploys. That closes a loop: the instance goes down, and the definition needed to restore it is inside the thing that is down.

Ordinary git already breaks the loop — every developer clone carries `.farrier/`, so a working copy on any machine is a complete bundle. The rule is that at least one such copy must exist somewhere the instance does not serve, and that the operator knows which one it is. A single-developer instance whose only clone is on a laptop that dies has lost its bundle as surely as one that kept it nowhere.

This is operator discipline rather than an enforced constraint. Secrets are unaffected: none of them lives in the bundle, and their custody is the keystore driver's ("Key custody").

## Farrier hosts Farrier

A later milestone: this repository moves onto an instance it deploys, by the same cutover.

It is gated on porting the agent fleet that develops this project off the GitHub API. The fleet runs as GitHub webhooks against GitHub's API, and Forgejo's surface differs — no GraphQL, no review-thread objects, commit statuses in place of check runs — so moving this repository before the port breaks the development loop. The porting notes live in `.claude/skills/farrier-conductor/SKILL.md` under "When this moves to Forgejo".

## Distribution and licensing

Open-source repository. Operators clone it and run it on their own hardware. The project ships software and documentation; hosting, operation, and support of deployed instances belong to the operator.

Farrier is licensed Apache 2.0: permissive enough that anyone can use, fork, or commercialize it, while requiring that the license and NOTICE travel with derivative works and that modifications are stated — attribution back to the project without restricting use. Apache 2.0 also grants patent rights explicitly, which MIT and BSD leave unaddressed. Components run as orchestrated containers rather than linked code, so Forgejo's GPL-3.0-or-later places no obligation on this project's own source.
