---
name: farrier-fleet
description: >-
  Batch-spawn n cloud agents: assign each to one PR (fix, polish, or merge
  conflict) or one new work chunk planned from status + functional-requirements order.
  Uses @farrier/fleet CLI and the Cloud Agent SDK. Use when Evan asks to kick off a
  fleet, spawn n agents in parallel, or run a batch conductor pass.
---

# Farrier Fleet

Batch counterpart to [farrier-conductor](../farrier-conductor/SKILL.md). One command
plans **`n`** assignments and spawns **`n`** agents.

> **This is the Cursor path**, and it needs `CURSOR_API_KEY`. Planning is shared
> — the same `@farrier/fleet` CLI — but the primary path now fires Claude Code
> Routines from `.claude/skills/farrier-conductor/SKILL.md`. Prefer that one.

## When to use

- Evan gives a number: "spawn 6 agents", "fleet pass", "batch conductor"
- Parallel PR fixes + new work chunks in one shot

## One fleet pass

1. **Choose `n`** from Evan's request (not capped at 2 — fleet is explicit batch).

2. **Get current first.** Fleet reads PRs live from the API but reads the
   backlog — `docs/status.json`, `docs/functional-requirements.md` — **off disk**. A
   stale checkout assigns agents to requirements that already shipped, and
   nothing in the plan output looks wrong when it happens:

   ```bash
   git fetch origin main
   git worktree remove --force /tmp/farrier-main-wt 2>/dev/null
   git worktree prune
   git worktree add --detach /tmp/farrier-main-wt origin/main
   ```

   `remove` + `prune` rather than `rm -rf`: deleting the directory leaves
   metadata under `.git/worktrees/` that makes the next `add` fail. Remove the
   worktree once the pass is done.

3. **Plan** (always do this first):

   ```bash
   pnpm --filter @farrier/fleet plan -- --n <count> --repo-root /tmp/farrier-main-wt
   ```

   `--repo-root` is what the backlog is read against — pass it rather than
   trying to run from the worktree. `pnpm --filter` runs in `tools/fleet`
   regardless of where you invoke it, so without the flag the plan reads the
   working tree's `docs/` no matter what directory you started in.

   Read the human summary. Confirm assignments look sane before spawning unless
   Evan said to run unconditionally. For every `implement` assignment, check the
   ID is genuinely unshipped on main:

   ```bash
   git show origin/main:docs/status.json | grep -o '"<ID>": *[^,}]*'
   ```

   `landed` means the plan is stale — re-do the worktree rather than spawning.

4. **Spawn**:

   ```bash
   pnpm --filter @farrier/fleet fleet -- --n <count>
   ```

   Use `--dry-run` if Evan only wanted the plan.

5. **Report** to Evan:
   - Each assignment (PR fix / PR polish / chunk IDs)
   - Agent URLs from spawn output
   - Any `ASK_PAUSE`
   - Skipped PRs (agent already in flight) and unfilled backlog slots
   - Do **not** merge anything

## Assignment rules (same defaults as conductor unless Evan overrides)

| Slot type | Trigger |
|---|---|
| PR fix | merge conflict, `conductor:changes-requested`, unresolved threads on a non-approved PR, red CI (oldest first) |
| PR polish | `conductor:approved` + open threads — docs nits, tech-debt nits, merge-time nits |
| New work | next ready **work chunk** from status + spec order + dependency gates |

Verdicts are **labels**, not GitHub review states. An agent review posts under
the repo owner's own account and GitHub forbids approving your own PR, so
`reviewDecision` is always `null` here and carries no information. See the
conductor skill for the full label table and the lock rules.

**Review and fix form a closed loop and it runs unbounded** — a review requests
changes, a fixer is queued, the fixer pushes, the push triggers another review.
There is no round cap and nothing parks a PR for having been round-tripped too
many times. Deciding a PR is stuck is Evan's call, not a counter's.

### Dependency order + full chunks

- **No static chunk catalog.** Each run plans from `docs/status.json`,
  functional-requirements order, and `dependenciesSatisfied` (dependency gates). **Default is one ID per agent.** Co-pack only
  explicit pairs (KEY-001+002, DNS-001+002, BLOB-001+002).
  Gates are encoded in `dependenciesSatisfied`.
- **One ID is not one PR.** An ID too large for a reviewable PR ships as
  declared serial slices: mark it `{"state":"partial","remaining":"<what is
  left>"}` and the next pass picks it back up carrying that note. A bare
  `"partial"` is a parse error — the note is the only part a later pass cannot
  work out by reading the repo.
- **Order:** needy PRs (oldest first, **never skipped for zone overlap**),
  then dynamically planned ready chunks on **non-overlapping domains/modules**.
- Open PRs already exist — assign every eligible fix/polish up to `n` even
  when two touch the same module. Conflict avoidance applies only to **new**
  implement chunks (and those chunks also avoid modules claimed by PRs in
  the same batch). Soft merge paths (`isSoftMergePath`) classify registry
  churn for path-hardness helpers only — see `tools/fleet/README.md`
  "Merge-conflict avoidance." Do not serialize the whole backlog on
  universal migration hotspots.
- Conflict skips: **fold one co-ship partner** into the selected implementer.
  Do not glue arbitrary same-domain neighbors.
- `ASK_PAUSE` only for malformed non-requirement tokens. Conflict-deferred
  ready chunks are `conflictAvoided`, not ask-pause.
- Exit code `3` means ask-pause (clear PR/chunk agents may still have spawned).

Polish briefs match conductor spirit: fix documentation, simplification, and
nits worth doing before merge — not scope expansion.

Implementer / fixer briefs baked into fleet include:

- **Read `docs/spec.md` before deciding anything.** The spec outranks the
  agent's judgement and settles questions `CLAUDE.md` never mentions.
- **Merge `origin/main` into the assigned branch first** — stay on the
  SDK-assigned branch; do not create a parallel "branch off main"
- **Push once** — implementers after implementation and tests; fix/polish after
  review feedback and tests (never push after a merge alone)
- Step back if the approach is not simple; change approach when it helps the future
- **Run the tests for the packages you touched, not the whole suite.** CI runs
  everything on every PR, and a merge to `main` fails
  if it fails. That is the gate — an agent re-running hundreds of tests locally
  buys nothing and costs tokens on every pass.

## Out of scope

- Auto-merge (never)
- Inventing thin slices or chunk boundaries when `ASK_PAUSE` fires
- Spawning without `CURSOR_API_KEY`
- Inventing stack or architecture answers, or renegotiating a settled decision

## See also

- `tools/fleet/README.md`
- `@farrier/conductor` for single-pass interactive conductor and `prs` / `status`
