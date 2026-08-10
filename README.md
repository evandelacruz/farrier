# Farrier

*A farrier travels with a portable forge — the whole shop packed to move, set up wherever the work is.*

Point Farrier at a project folder and it stands up a self-hosted forge for it — git hosting, pull requests, code review, secrets, and CI/CD with runners — with a remote ready to push to. Each project gets its own portable forge, not a shared central server. One command brings it up. One command backs it up. One command restores it onto a fresh host anywhere.

## Quick start

### 1. Build the binary

You need Go (version in `go.mod` — currently 1.24.7). Nothing else: Farrier is one static binary with no runtime dependencies.

```bash
git clone https://github.com/evandelacruz/farrier.git
cd farrier
go build -o farrier ./cmd/farrier
```

Optionally put it on your `PATH` so the commands below work from any project folder:

```bash
sudo mv farrier /usr/local/bin/          # or: mv farrier ~/.local/bin/
farrier --help
```

### 2. Have a host ready

You bring a host with **Docker and SSH** and a place to keep keys. Farrier installs nothing on that host — it opens an SSH session, does the work, and exits.

**Running it on your own machine counts**, and is the fastest way to try it. That means Docker running locally, plus an SSH server you can reach as yourself:

```bash
ssh localhost docker info      # both prerequisites, in one check
```

If that fails, you need `sshd` running and your own key in `~/.ssh/authorized_keys`, and your user in the `docker` group. Then target `ssh://you@localhost` below.

A DNS name is optional: with one, the forge gets HTTPS and relocates by DNS flip; without one, it comes up in a minute on plain HTTP and you can attach a name later.

### 3. Make the project a forge

`init` generates all key material and writes the bundle to `.farrier/` beside your code. Driver paths must be absolute — they are re-resolved from whatever directory a later command runs in.

```bash
cd my-project

farrier init \
  -keystore-driver file -keystore-config path="$HOME/.farrier/keys" \
  -blob-driver local    -blob-config    path="$HOME/.farrier/blobs"
```

That is a **nameless** bundle: nothing to own, no DNS, no certificate. To get HTTPS and a permanent identity instead, add a name and the DNS provider that proves it — credentials come from that provider's own environment variables:

```bash
farrier init \
  -domain myproject.example.com -acme-dns-provider cloudflare \
  -keystore-driver file -keystore-config path="$HOME/.farrier/keys" \
  -blob-driver local    -blob-config    path="$HOME/.farrier/blobs"
```

**Read what `init` prints.** It names where every piece of key material went, and which one you cannot lose.

### 4. Deploy it

```bash
# Named bundle — live at your domain over HTTPS
farrier up -bundle .farrier -target ssh://you@host

# Nameless bundle — no domain, so tell `up` where it is reached.
# Plain HTTP: keep it on a LAN, a VPN, or a tailnet.
farrier up -bundle .farrier -target ssh://you@localhost -address 192.168.1.5
```

`up` prints the first admin account's credentials exactly once, through the event stream. Save them.

### 5. Push your project to it

Create an access token in the forge's web UI, then:

```bash
export FARRIER_TARGET_TOKEN=...        # never passed as a flag

# Named instance — the target defaults to https://<the bundle's domain>
farrier publish

# Nameless instance — name the address you deployed at
farrier publish -target http://192.168.1.5
```

That creates the repository, pushes your existing history, and points `origin` at the instance. `git push` works normally from here on.

### 6. Back it up

```bash
farrier backup -bundle .farrier -target ssh://you@host -to ./backups
# ...or -to s3://my-bucket/farrier
```

Then prove it restores, before you need it to: `farrier drill -bundle .farrier -target ssh://you@scratch-host -from ./backups`.

### Later: give a nameless instance a name

In place, losing nothing but the clone URLs — which it reports, so you can tell your team what to re-point.

```bash
farrier attach -bundle .farrier -target ssh://you@host \
  -domain myproject.example.com -acme-dns-provider cloudflare \
  -address 192.168.1.5
```

Day-to-day work stays in Forgejo's UI at your domain. Portability — status, drills, promote — stays on your machine (`farrier status`, `farrier drill`, `farrier promote`, or `farrier ui`). Backing up, restoring, and getting back after losing a host: [docs/operating.md](docs/operating.md). What you are accepting security-wise: [docs/security.md](docs/security.md). Full command set: [docs/spec.md](docs/spec.md).

**Keep the age backup key.** `init` reports where it stored every piece of key material and which one matters most: the age key alone decrypts every snapshot this instance will ever produce, and there is no recovery path if it is lost. Put a copy somewhere other than the machine you would be recovering from.

## The problem

Your development pipeline runs on infrastructure you don't control. When your forge or CI provider has an outage, you wait. You can't ship, can't merge, can't deploy, and there is nothing you can do about it except refresh a status page. Self-hosting solves the control problem but replaces it with a new one: your forge is now welded to one machine at one provider, and moving it is a weekend of undocumented archaeology.

## The solution

Treat the entire forge as a portable bundle with a stable identity:

- **Stateless and stateful layers are split.** The forge app, CI orchestrator, and runners are disposable — they relocate in seconds. State (repos, database, secrets, artifacts) lives in one primary place with a clean, documented interface for replication.
- **Identity belongs to the bundle, not the host.** Every bundle owns a DNS name and its own key material (encryption keys, TLS, SSH host keys). Relocation is a DNS flip. Hosts are fungible.
- **Restore is verified, not assumed.** The CLI knows exactly what constitutes complete state and refuses to promote from a torn or partial snapshot. A rehearsal command restores your latest backup to a scratch target and proves it boots and runs CI.
- **The control plane lives on your machine.** One core engine with two thin frontends — a CLI and a local web dashboard — so status, backups, and the promote button all work when the forge host is down.

Built on existing open source (Forgejo, Forgejo Actions). This project is the orchestration and portability layer: the CLI, the bundle definitions, and the migration path.

## The trade

- **You host it.** There is no service behind this project. You bring a host with Docker and SSH, a domain you control, and you operate it yourself.
- **Outages become actionable.** When your provider goes down, you promote a standby and flip DNS instead of waiting. Failover is a manual decision with a small, visible data-loss window and a few minutes of downtime.
- **You get off the GitHub milk.** Your repos, reviews, secrets, and CI live on infrastructure you can pick up and move — to another provider, another country, or the machine under your desk.

## Scope

Git hosting, pull requests, code review, secrets, and CI/CD with runners. Nothing else — no issues, no wikis, no project management.

**Built for private repositories.** The target is a team's own work: internal services, client projects, proprietary code. If you run a public open-source project, GitHub or Codeberg is probably still the better home — their value is the network (discovery, drive-by contributors, an identity contributors already have), and that is not something self-hosting can replace. Public repos work here; they just aren't what this optimizes for.

## License

Apache 2.0 — use it, fork it, ship it commercially. It requires attribution: keep the license and NOTICE, and state what you changed. It also grants patent rights explicitly, which a bare MIT license does not.

## Status

The forge and the whole portability layer — `up`, `backup`, `restore`, `promote`, `upgrade`, `drill`, `status`, `ui` — are landed. `up` serves git over SSH on the port the bundle declares, so clone and push work against a fresh deployment. The project-folder on-ramp is not landed: `init` does not yet write to `.farrier/` or run without a domain, and there is no path from a local folder to a working `origin`. Delivery state: [docs/status.json](docs/status.json). Design reference: [docs/spec.md](docs/spec.md).
