# Farrier

*A farrier travels with a portable forge — the whole shop packed to move, set up wherever the work is.*

A CLI and local web dashboard that stand up a portable, self-hosted software forge — git hosting, pull requests, code review, secrets, and CI/CD with runners — as a provider-agnostic bundle. One command brings it up. One command backs it up. One command restores it onto a fresh host anywhere.

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

Pre-implementation. See [docs/spec.md](docs/spec.md) for the design reference.
