# Farrier

*A farrier travels with a portable forge — the whole shop packed to move, set up wherever the work is.*

Farrier is a single-binary orchestrator that stands up a self-hosted Forgejo instance — one per project by default, or one serving several — where Forgejo (and its Actions runner) does the actual git hosting, pull requests, code review, and CI, while Farrier pins it into a versioned bundle, deploys it onto any Docker-plus-SSH host, and owns everything Forgejo doesn't: backup, restore, upgrade, drill, and DNS failover.

## Quick start

### 1. Build the binary

You need Go 1.24.7 or newer (the version in `go.mod`). Nothing else: Farrier is one static binary with no runtime dependencies.

```bash
go version        # already have it? skip ahead
```

If not, install it from [go.dev/dl](https://go.dev/dl/) — official builds for macOS, Linux, and Windows, with install steps at [go.dev/doc/install](https://go.dev/doc/install). Or use a package manager, as long as it gives you 1.24.7+:

```bash
brew install go              # macOS
sudo apt install golang-go   # Debian/Ubuntu — check `go version`, distro packages lag
```

Then build:

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
ssh localhost 'command -v docker >/dev/null || PATH="$PATH:$HOME/.docker/bin:/usr/local/bin:/opt/homebrew/bin:/snap/bin"; docker info' >/dev/null && echo ok
```

That is deliberately the same thing Farrier does: reach the host over SSH, and if `docker` is not on that session's PATH, look in the handful of places installers actually put it. A plain `ssh localhost docker info` is **not** the right check — it fails on a stock macOS Docker Desktop install that Farrier handles fine, because an SSH command session reads none of the startup files that put `~/.docker/bin` on your PATH.

What this check tells you, and your interactive terminal cannot: whether SSH lets Farrier in, and whether Docker is reachable once it is.

If it prints `ok`, target `ssh://you@localhost` below and skip the rest of this step. If not, the fix depends on how it failed.

**No SSH server.** Farrier connects over SSH even when the host is the machine you are sitting at — locality is an argument, not a mode, so there is no local shortcut that skips it.

```bash
# macOS: no install needed, just enable it
sudo systemsetup -setremotelogin on
# ...or System Settings → General → Sharing → Remote Login

# Debian/Ubuntu
sudo apt install openssh-server && sudo systemctl enable --now ssh

# Fedora/RHEL
sudo dnf install openssh-server && sudo systemctl enable --now sshd

# Arch
sudo pacman -S openssh && sudo systemctl enable --now sshd
```

Then authorize your own key, since Farrier uses your SSH agent or a key file and never a password:

```bash
[ -f ~/.ssh/id_ed25519 ] || ssh-keygen -t ed25519      # if you have no key
cat ~/.ssh/id_ed25519.pub >> ~/.ssh/authorized_keys
chmod 600 ~/.ssh/authorized_keys

ssh-add --apple-use-keychain ~/.ssh/id_ed25519         # macOS; plain ssh-add elsewhere
ssh localhost true                                     # accept the host key once
```

Both of those last two lines matter, and each fails in its own confusing way.

**Load the key into your agent.** Farrier authenticates through your SSH agent, or a key file you name with `-ssh-key`. Plain `ssh` also reads `~/.ssh/id_ed25519` off disk on its own and can fall back to a password, so `ssh localhost` succeeding does **not** mean Farrier will connect — an empty agent fails with `attempted methods [none publickey]`. `ssh-add -l` shows what the agent holds.

**Accept the host key once.** An unrecorded host key fails the connection rather than prompting, because Farrier's jobs run unattended.

**`command not found: docker`.** Farrier already searches `~/.docker/bin`, `/usr/local/bin`, `/opt/homebrew/bin`, and `/snap/bin` when a session's PATH has no `docker`, so this means yours is somewhere else. Put it on the PATH that non-interactive shells see — for zsh that is `~/.zshenv`, not `.zshrc` or `.zprofile`, which such a session never reads:

```bash
echo 'export PATH="/where/docker/lives:$PATH"' >> ~/.zshenv
```

**No Docker at all.** Install it from [docs.docker.com/engine/install](https://docs.docker.com/engine/install/) (Linux) or [docker.com/products/docker-desktop](https://www.docker.com/products/docker-desktop/) (macOS/Windows), then let your user reach it without `sudo`:

```bash
sudo usermod -aG docker "$USER"    # Linux; log out and back in for it to apply
```

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

A nameless instance comes up at **`http://<address>:8222`**, and a named one at **`https://<domain>`**. Those ports are the bundle's, not Farrier's: your host may already be serving something on 80 or 443, so set `-web-port` at `init` if you need it somewhere else. If something on the host holds the standard port and forwards to Farrier, add `-public-web-port` so every URL the forge renders names the port your clients actually connect to — and make sure that forwarder passes TCP through, since Farrier terminates TLS with the bundle's own certificate.

**Deploying to your own machine? Add `-remote-dir`.** `up` writes configuration and forge state into `/opt/farrier` by default — fine on a dedicated host you reach as root, and not writable by an ordinary user:

```bash
farrier up -bundle .farrier -target ssh://you@localhost -address 192.168.1.5 \
  -remote-dir /Users/you/farrier          # Linux: /home/you/farrier
```

**Still give your machine's real address, not `127.0.0.1`.** The address is what the forge tells CI to clone from, and CI jobs run in containers — inside one, a loopback address is the container itself, so every workflow fails at its first step while the instance looks perfectly healthy. `up` refuses a loopback address for that reason and names one your machine answers on. (An instance you only browse has no such container: turn CI off with `colocatedRunner: false` in the bundle's `farrier.yaml` and `127.0.0.1` is fine.)

Three things about that path:

- **One bundle per directory.** Everything an instance keeps on the host lives under it, and pointing a second bundle at the same directory hands that state to the wrong forge. `up` does not yet catch this, so give each instance its own path.
- **Give it absolutely, never as `~/farrier`.** It is quoted into a command on the host, so a tilde is taken literally and you get a directory actually named `~`.
- **On macOS, keep it under `/Users`.** `up` bind-mounts forge state from this directory into the containers, and Docker Desktop shares only a fixed set of host paths with its VM. `/Users` is shared by default; `/opt` is not, so `/opt/farrier` would fail at mount time even with the permissions fixed.

Whatever you choose, pass the same `-remote-dir` to every later command against that host — `backup`, `status`, `drill`, `upgrade` all default to `/opt/farrier` too.

`up` prints the first admin account's credentials exactly once, through the event stream — its password, and an access token for the next step. Save them.

### 5. Push your project to it

Export the token `up` printed:

```bash
export FARRIER_TARGET_TOKEN=...        # never passed as a flag

# Named instance — the target defaults to the bundle's own public URL
farrier publish

# Nameless instance — name the address you deployed at
farrier publish -target http://192.168.1.5:8222
```

That creates the repository, pushes your existing history, and points `origin` at the instance. `git push` works normally from here on.

**It registers an SSH public key on your forge account** when that account has none — which a brand new instance never does, and a push to an account with no key is rejected. It takes your own key, `~/.ssh/id_ed25519.pub` or failing that `~/.ssh/id_rsa.pub`, and names the file it registered in its output. `-ssh-key /path/to/some_key.pub` names a different one. An account that already has a key registered is left alone.

### 6. Back it up

```bash
farrier backup -bundle .farrier -target ssh://you@host -to ./backups
# ...or -to s3://my-bucket/farrier
```

Then prove it restores, before you need it to: `farrier drill -bundle .farrier -target ssh://you@scratch-host -from ./backups`.

### Later: give a nameless instance a name

In place, losing nothing but the clone URLs — which it reports, so you can tell your team what to re-point. The web port moves with the name: `http://<address>:8222` becomes `https://<domain>`.

```bash
farrier attach -bundle .farrier -target ssh://you@host \
  -domain myproject.example.com -acme-dns-provider cloudflare \
  -address 192.168.1.5
```

Day-to-day work stays in Forgejo's UI at your domain. Portability — status, drills, promote — stays on your machine (`farrier status`, `farrier drill`, `farrier promote`, or `farrier ui`). Working on the instance day to day — where CI workflows go, how secrets are set, putting a second project on it: [docs/using.md](docs/using.md). Backing up, restoring, and getting back after losing a host: [docs/operating.md](docs/operating.md). What you are accepting security-wise: [docs/security.md](docs/security.md). Full command set: [docs/spec.md](docs/spec.md).

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

**All twelve commands ship; three known gaps are open; real use is just starting.**

73 of 76 requirements are implemented, reviewed, and tested — the twelve commands, the project-folder on-ramp, the nameless tier, and the whole portability layer. Delivery state, one line per requirement: [docs/status.json](docs/status.json).

The three open ones came out of the first end-to-end runs against real hosts:

- `up` does not yet refuse to deploy a bundle onto host state belonging to a different bundle, so keep one instance per host directory until it does.
- Some failures do not say what to do about them.
- CI does not yet run `init`, `up`, and `publish` against a real Docker daemon.

Those runs are also turning up what unit tests structurally cannot: a default directory an ordinary user cannot write, an image tag that did not exist, a `chown` that is mandatory on Linux and impossible on macOS. Expect to hit rough edges on a first deployment, and expect them in the seams between components rather than inside them.

Nothing here is abandoned or speculative — it is young. Design reference: [docs/spec.md](docs/spec.md).
