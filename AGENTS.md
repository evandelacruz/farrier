# AGENTS.md

[CLAUDE.md](CLAUDE.md) is the primary operating guide — the doc hierarchy, the
negotiability rule, the house patterns, and scope. Read it first. This file adds
only what a cloud-agent environment needs on top.

## What this repo is

Two languages, one product:

- **The product is Go.** `cmd/farrier` (thin CLI), `internal/core` (all logic),
  `internal/api` (loopback server), `web/` (dashboard, embedded via `go:embed`).
  `go build ./...` produces the binary with no Node involvement.
- **`tools/` is agent orchestration, in TypeScript.** The conductor and fleet
  CLIs plan agent assignments and fire routines. Dev-only scaffolding: it never
  ships in the binary and is not part of the product build.

Work on a requirement touches the Go side. Work on how agents are dispatched
touches `tools/`. They are independent.

## Setup

**Go** — the version is pinned in `go.mod`; nothing else to install.

```bash
go build ./...
go test ./internal/core/<pkg>/...   # scoped to what you touched
```

**Agent tooling** — pnpm workspace, Node >= 22.

```bash
pnpm install
pnpm --filter @farrier/conductor build   # fleet depends on its declarations
pnpm -r typecheck && pnpm -r test
```

`@farrier/fleet` imports `@farrier/conductor` as a workspace dependency, so a
typecheck of fleet fails with "cannot find module" until conductor has been
built once. This is the one non-obvious ordering in the repo.

## Test scope

Run the tests for what you touched, not the whole suite. CI runs the build and
everything else on every PR into main, and a merge fails if it fails — that is
the gate. Writing tests for behavior you add or change is untouched by this;
executing the entire suite locally is what it rules out.

## The work list

- `docs/functional-requirements.md` is the backlog. Stable IDs (`BKUP-002`,
  `RSTR-001`). Never renumber one.
- `docs/status.json` is the delivery record: `landed` / `open` /
  `{"state":"partial","remaining":"<what is left>"}`. A bare `"partial"` is a
  parse error — the note is the only part a later pass cannot work out by
  reading the repo.
- It is a work list, not a source of truth. `docs/spec.md` and the README are.
  Where status disagrees with the spec, status is what is wrong.

Both files are read **off disk**, so a stale checkout invents work that already
shipped. Plan from a fresh `origin/main` worktree — see the conductor skill.

## Cloud environment notes

- The repo lives at `github.com/evandelacruz/farrier`. GitHub hosts the build
  for now. "Farrier hosts a real project" is the dogfooding milestone — a
  private project, not this repository — and docs/spec.md holds its cutover
  sequence and what counts as met: landing one pull request end to end on the
  new instance, then `backup` and `drill`. `import` is step 2 of that sequence,
  not the test. "Farrier hosts Farrier" is a later milestone, gated on porting
  this fleet off the GitHub API.
- Open PRs **ready for review, not draft.** If the tooling defaults to draft,
  set `draft: false` or run `gh pr ready` before finishing.
- Never merge. Evan merges.
- Review verdicts are labels (`conductor:approved`,
  `conductor:changes-requested`), not GitHub review states — agent reviews post
  under the repo owner's account and GitHub forbids approving your own PR. Add
  and remove labels individually; sending a replacement label set wipes a lock
  another agent is holding.
