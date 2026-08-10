# CLAUDE.md

Operating guide for agents working in this repository.

## What this project is

Farrier: a CLI and local web dashboard that stand up a portable, self-hosted forge (git, PRs, review, secrets, CI/CD) as a provider-agnostic bundle, built on Forgejo. One command up, one command to back up, one to restore anywhere. Read the [README](README.md) first.

## The documents and their jobs

Each doc has one function. Content lives in exactly one place; the others link to it.

| Doc | Job |
|---|---|
| [README.md](README.md) | The public promise: problem, solution, trade. |
| [docs/spec.md](docs/spec.md) | The decision record: every settled design decision, what and how. **Source of truth**, with the README, when documents disagree. |
| [docs/functional-requirements.md](docs/functional-requirements.md) | Observable behavior, stated testably, with the stable requirement IDs. The WHAT. |
| [docs/tech-spec.md](docs/tech-spec.md) | Internal structure, formats, protocols, operational targets — only what no single package owns. The HOW. |
| [docs/operating.md](docs/operating.md) | The operator's runbook: recovery, backups, restore, drill, upgrade. Task-shaped, not design. |
| [docs/status.json](docs/status.json) | Delivery record — one line per requirement ID. A work list, not a source of truth. |
| CLAUDE.md | This file: how to work here. |
| [AGENTS.md](AGENTS.md) | Cloud-agent environment notes: setup, build ordering, test scope. |

## Rules

### Everything is negotiable — with Evan, and only with Evan

Every decision in these docs can be reopened at any time, including mid-implementation — by Evan. Agents do not renegotiate decisions with themselves. If implementation reveals that a documented decision is wrong, costly, or contradictory: stop, state the problem and a proposed alternative, and wait for Evan's call. Never silently deviate and never let code drift ahead of the docs.

### Docs stay in sync

Any change to one doc requires checking the others and updating whatever is needed to stay consistent. A PR that changes behavior updates the affected docs in the same PR. If you find documents already in conflict, flag it — spec.md and the README win, but the conflict gets fixed, not worked around.

**In sync does not mean exhaustive.** Implementation detail belongs in the package's own doc comments, next to the code, where it cannot drift. A requirement landing is not a reason to add a section to tech-spec.md — only a change to a format, a protocol, an operational target, or the package layout is. Docs that restate the code are the ones that go stale, and every doc-sync defect this project has hit came from exactly that.

### Scope is a feature

Git hosting, PRs, code review, secrets, CI/CD, and the portability layer. No issues, no wikis, no project management. Do not add scope; do not build toward it speculatively. The target user runs private repositories — public-project features (discovery, drive-by contribution, social identity) are not what this optimizes for.

### House patterns

- **One core, thin skins.** All logic in `internal/core`; the CLI and dashboard contain zero logic. New capability goes in the core and is exposed through both frontends via the shared event stream.
- **Plugin posture.** Extensibility ships as a published driver interface (Go interface + exec/JSON protocol) with one or two first-party drivers — the pattern used by DNS, keystore, and blob. New extension points follow it.
- **Verification is load-bearing.** Backup and restore share one verification code path. Anything that weakens "a backup that fails verification fails loudly" is a design change for Evan, not a refactor.
- **Operator's machine is the control plane.** Nothing may require code running anywhere except the operator's machine and the target hosts over SSH.

## Development

- **Language:** Go, single static binary, dashboard embedded via `go:embed`. `tools/` is TypeScript agent orchestration — dev-only, never shipped in the binary. See [AGENTS.md](AGENTS.md) for setup and the one build-ordering gotcha.
- **The backlog:** requirement IDs in `docs/functional-requirements.md`, delivery state in `docs/status.json`. Cite IDs in commits and PR bodies. Never renumber an ID.
- **Repo host:** GitHub for now; agents work in branches and land through PRs with review and green CI. "Farrier hosts Farrier" is a named milestone; [spec.md](docs/spec.md) holds its cutover sequence and what counts as met — landing one pull request end to end on the new instance, then `backup` and `drill`. `import` is step 2 of that sequence, not the test.
- **License:** Apache 2.0. Keep the license and NOTICE intact.
- **Writing style in docs:** crisp, affirmative statements, high-level context first with details beneath, plain-English headlines. State what the system does; skip editorial hedging.
