# `@farrier/fleet`

Batch orchestrator for Farrier: given **`n`**, plan **`n`** agent assignments — one
open PR or one work chunk each — and emit a dispatch manifest for the conductor
to fire.

Planning and spawning are separate on purpose. Planning needs the repo and
GitHub; spawning needs whatever can start an agent. Keeping them apart is what
lets the same tested planner drive Claude Code Routines today and something
else later.

## Setup

```bash
pnpm install
pnpm --filter @farrier/conductor build
pnpm --filter @farrier/fleet build
```

## Commands

```bash
# Plan, printing assignments and the reason every other PR was passed over
pnpm --filter @farrier/fleet plan -- --n 6

# Plan from PR data the caller already has (no `gh` needed), writing a manifest
node tools/fleet/dist/cli.js plan --n 6 --prs prs.json --out .fleet-run

# Skip review assignments (a GitHub trigger fires the reviewer instead)
node tools/fleet/dist/cli.js plan --n 6 --no-reviews
```

Run from any directory inside the repo; the CLI walks up to find `CLAUDE.md`.

### `--prs` — planning without `gh`

By default the planner shells out to `gh`. **A Claude Code session has GitHub
MCP tools and no `gh` binary**, so the conductor gathers open PRs itself and
passes them in as a JSON array of `PrCommentSummary` objects. The exact field
list and the normalization traps (lowercase `mergeable_state`, derived
`reviewDecision`, `PENDING` reviews) are in
`.claude/skills/farrier-conductor/SKILL.md`.

## Worker roles

| Role | Model | Does |
|---|---|---|
| implementer | `opus` | new work chunks, PR fixes, PR polish — writes code, pushes once |
| reviewer | `sonnet` | reads a PR, posts exactly one review, never pushes |

Models are aliases rather than pinned ids, so `opus` always means the current
Opus. Override with `FARRIER_IMPLEMENTER_MODEL` / `FARRIER_REVIEWER_MODEL`.

"Never pushes" means never pushes **code**. A reviewer still writes GitHub
issue labels — that is how it records its verdict (see "Verdict labels stand
in for the review state" below) — over the same App-installation-token
identity every Routine gets from the egress proxy. That is also the channel
`conductor:reviewing` is dropped through when the review is posted: a
reviewer able to post `conductor:approved` / `conductor:changes-requested` is
already able to remove its own lock label the same way.

## PR routing

Evaluated per open PR, in order. The first match wins:

| Condition | Outcome |
|---|---|
| `conductor:working` or `conductor:reviewing` label | skip — agent in flight |
| merge conflict / changes requested / unresolved threads / failing checks | **fix** |
| approved with open threads | **polish** |
| approved, no conflicts, nothing open | skip — Evan merges |
| draft | skip |
| head commit carries no submitted review | **review** (unless `--no-reviews`) |
| otherwise (already reviewed at head) | skip |

Two properties worth keeping:

- **"Needs review" is derived from state, not events.** The head SHA is
  compared against the commit SHAs of submitted reviews, so a push re-opens a
  PR for review by itself. Polling and event delivery differ only in latency,
  which is what lets the conductor run on a schedule without missing work.
- **Fix rules run before the draft check.** A draft with a merge conflict is
  still broken; only *reviewing* a draft is wasted work.

`checksOk` is `null` when no checks ran **or when they are still running**, and
`null` is not failure — only an explicit `false` queues a fixer. CI runs on
every PR into `main` (`.github/workflows/ci.yml`), so a red build now queues a
fixer on its own. Read it with the `pull_request_read` / `get_check_runs` MCP
tool; `curl` against `/status` or `/check-runs` returns 403 for the session
token, and a 403 parsed as "no checks" would hide every failure.

### Review identity — why reviews never approve

Agent reviews post under **the repo owner's GitHub identity**, and GitHub does
not let an account approve or request changes on a PR it authored. So a review
lands as a `COMMENTED` event saying "treat this as blocking", and
`reviewDecision` is permanently `null`.

This is not a configuration miss. Claude Code cloud sessions reach GitHub
through an egress proxy that **replaces the `Authorization` header on every
request to `api.github.com`** with a GitHub App installation token bound to the
account. Verified directly: a deliberately fake token still returns `200` and
resolves to the owner, as does sending no auth header at all, and the reported
rate limit is 15000/hr — an App installation ceiling, not a PAT's 5000. `gh`,
`curl`, MCP, `Bearer`, `token`, and basic auth all land in the same place, and
an injected PAT for a second account changes nothing. Anthropic documents the
behavior: *"Anything a routine does through your connected GitHub identity
appears as you."*

#### Verdict labels stand in for the review state

Labels are not subject to the self-review restriction — an author can label
their own PR — so the reviewer records its verdict as a label and the conductor
reads that wherever it would have read `reviewDecision`:

| Label | Means |
|---|---|
| `conductor:approved` | nothing blocking; ready for a human to merge |
| `conductor:changes-requested` | blocking findings; queue a fixer |

`effectiveReviewDecision()` prefers a real GitHub decision and falls back to
these. So if a human ever reviews, or reviews start coming from a separate
identity, that truth wins automatically and the labels become a no-op — no code
change needed. When both labels are present, `changes-requested` wins: the
conservative reading is the one that keeps working on the PR.

This is our state machine, not GitHub's. **Branch protection still cannot gate
on it**, and that is the one thing the workaround does not buy back.

**A verdict label does not survive a push.** An approval describes the commit it
was written against, but a label sits on the PR indefinitely and cannot know new
code arrived. So `approved-and-clean` also requires the head commit to carry a
review — approve, then push, and the PR goes back for review rather than
skipping. Every push gets reviewed, which is the whole point of the trigger.

Escape hatches, if a *formal* review state ever matters: run the reviewer
**locally** (Remote Control), where the credential is whatever `GH_TOKEN` says;
have a post-only GitHub Action publish the review under a second account's PAT;
or use another vendor's review bot, which has its own App identity. All were
considered and rejected as more machinery than a formal state is worth here.

### Reviews are webhook-driven; the conductor runs `--no-reviews`

The reviewer Routine is bound to a `Pull request: Commits pushed` webhook, so a
push reviews itself. **Pass `--no-reviews`**, or every push is reviewed twice
and half the batch is spent on work that was already coming for free. The
conductor skill requires the flag; do not omit it to "force" reviews into the
plan.

When the webhook misses (rate limit, orphaned pending review, no event), the
conductor's **§3b** backfill fires the reviewer for any PR whose head has no
verdict-carrying review and no `conductor:reviewing` lock. That is the
self-heal — not putting reviews back into `plan`.

That closes a loop: review requests changes → conductor fires a fixer → fixer
pushes → webhook (or §3b) fires the next review. **The loop has no round cap.**
It runs until the PR converges or Evan intervenes; the planner counts nothing
and queues a fixer every pass for as long as something is open. Deciding that a
disagreement is stuck is a human call — Evan is watching the sessions and can
stop one whenever he wants, which is cheaper than a counter that parks a PR
nobody asked it to park.

### In-flight locks

`conductor:working` / `conductor:reviewing` are PR labels. The conductor is
stateless and re-derives the whole plan each pass, so a worker in its own
session has no other durable way to claim a PR. The lock covers the window
between firing a worker and that worker pushing or posting — exactly when a
second pass would double-assign.

The conductor takes the lock; the worker drops it as its last action. If a
worker dies first, the label is stale and that PR is parked until someone
removes it by hand. That is a deliberate trade, not an oversight.

## How assignments are chosen

**Order (priority):**

1. **Open PRs first** — oldest PR number first, per the routing table above.
   Severity is only a same-number tie-break. Open PRs always outrank new
   backlog work: a growing PR queue starving new features is intended.
2. **Backlog chunks** planned **each run** from `docs/status.json` (what is
   still `open` / `partial`) + `docs/functional-requirements.md` order + the dependency
   gates encoded in `dependenciesSatisfied` (core foundation, capture
   core foundation before everything, capture prerequisites before backup,
   paired-driver prerequisites). **Default is one ID per agent.** Co-pack only
   for explicit pairs (KEY-001+002, DNS-001+002, BLOB-001+002) — not greedy
   same-domain adjacency. Promote, upgrade, and drill stay blocked until
   restore is landed.
   Parallel agents stay on non-overlapping domain/module zones. There is
   **no** hand-maintained chunk catalog to keep in sync with the backlog.

Does **not** invent thin unlock slices — each assigned ID is full MUST scope.

**Completion passes.** Mark a thin ship as `partial` in `docs/status.json`
(not `landed`). `partial` is queued like `open` so the completion pass runs.
No separate “reopen” catalog entry.

### Serial slicing — one ID across several PRs

An ID whose MUST is too large to review in one sitting may be delivered as
**serial** slices: one agent at a time, each PR complete for what it covers,
the ID left `partial` until the last one lands. The planner needs no new
state for this — `partial` is already queued like `open`, so a sliced ID comes
back on a later pass automatically.

Two rules make this a slice rather than a stub, and both live in the
implementer brief:

- **Declared before implementing.** The PR names every slice, including the
  deferred ones. A boundary discovered by running out of steam is a stub; one
  chosen up front is a plan. If an agent cannot state the deferred slices
  concretely, the ID is not sliceable — implement it whole or halt.
- **Split by capability, never by layer.** A vertical cut (schema + service +
  API + tests for one behavior) leaves `main` working and reviewable on its
  own merits. "All the migrations now, the API next" lands dead code and
  cannot be reviewed for correctness at all.

Slices are **serial, not parallel**. Two agents on one ID would share a
module, the registry files, and the migrations directory — precisely the
overlap `conflict-zones.ts` exists to prevent. Parallelism comes from running
different IDs at once, which the planner already does.

Where the ID picks up next is carried by the `remaining` line on its
`docs/status.json` entry, and the planner quotes that line verbatim into the
completion pass's brief. A bare `"partial"` is a parse error, so the line
cannot be dropped. The brief tells a
completion-pass agent to read it.

**ASK_PAUSE.** Only for a malformed non-requirement fallback token. Ordinary
open work is always plannable. Ready chunks deferred only for merge-conflict
avoidance leave slots empty and count under `conflictAvoided` — that is
**not** `ASK_PAUSE`.

**Merge-conflict avoidance (new work only).**

- **Open PRs are never skipped for zone overlap.** Fix/polish candidates are
  taken oldest-first up to `n`, same module or not — the PR already exists;
  agents resolve conflicts on their own branches. Only the routing table above
  skips a PR. Reviewers claim no zones at all, since they write no code.
- Soft shared paths (`isSoftMergePath` in `src/conflict-zones.ts`) still
  classify registry churn for path-hardness helpers (`prParallelZones` /
  `pathZoneSet`): CLAUDE.md / AGENTS.md / any README, `app.module.ts`,
  `docs/status.json`, `packages/db/src/index.ts`,
  `packages/db/src/schema/index.ts`, `packages/db/src/schema/enums.ts`,
  `packages/db/src/default-roles.ts`, worker registry (`handlers.ts` /
  `handlers.test.ts` / `worker-loop.ts`), and `pnpm-lock.yaml`. They do
  **not** gate which open PRs get agents.
- **Chunks:** domain / Nest-module / schema zones (e.g. `api:auth` vs
  `api:jurisdiction`). New implementers also avoid modules claimed by PRs
  selected in the same batch. Chunks do **not** all claim universal
  migration / `app.module` hotspots — that used to collapse every batch to
  one agent. Distinct migrations are now distinct timestamped files, so two
  agents each adding one touch disjoint paths and there is nothing to
  renumber; exact path zones still catch two PRs editing the *same* migration.
  Cross-domain `app.module` / `docs/status.json` touches are merged by agents.
- **Co-ship** ready chunks that collide (KEY-001 then KEY-002) are
  **folded** into one implementer (one extra partner, one PR). Arbitrary
  same-domain neighbors are not folded together.

### `docs/status.json` landed-ID contract

Fleet reads delivery state from `docs/status.json`, which carries one entry per
requirement ID in `docs/functional-requirements.md`:

- `landed` — shipped; never queued
- `partial` — a thin slice shipped; **queued** so the completion pass happens
- `open` — not started

Keep it current in the PR that lands the work, or fleet will re-queue shipped
IDs. Unknown states and unknown ID formats throw rather than defaulting, since
guessing either way silently corrupts the queue.

This replaced scraping backticked IDs out of status prose, which made
paragraph wording load-bearing — a forward reference to an ID inside the
Landed section marked it shipped while the same
section listed it as unbuilt. Status is data now, with no prose companion to
drift from it.

Each implementer brief includes the quality bar and an **internal self-review
cycle** (diff as a hostile reviewer → fix → re-test → repeat until happy) before
the single push. That critique stays in the agent’s notes — do not post review
comments on the PR for it. Same cycle is in fix/polish briefs.

**Branch + push semantics differ by assignment kind:**

- **Implementers** branch from latest `origin/main` onto the branch named in
  their assignment (`claude/<ids>`), and push once after implementation, tests,
  and self-review.
- **Fix / polish** agents check out the PR branch, merge `origin/main` into it,
  and push once after review feedback is addressed and tests pass.
- **Reviewers** never push.

Push is what re-opens a PR for review — never push after a merge alone.

## Policy

- Never auto-merge. Evan merges.
- PRs open **ready for review**, not draft.
- Halt on architecture / accounting / legal ambiguity — do not invent.

## Skills

- `.claude/skills/farrier-conductor/SKILL.md` — the conductor pass: gather PRs via
  GitHub MCP, plan, take locks, fire Routines.
- `.cursor/skills/farrier-fleet/SKILL.md` — legacy Cursor spawn path.

## Tests

```bash
pnpm --filter @farrier/fleet test
```
