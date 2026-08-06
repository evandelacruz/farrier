# `@farrier/conductor`

Thin CLI that helps a Cursor agent (or you) play **Farrier build conductor**:
spawn implementer cloud agents from the functional spec, follow up on review
fixes, and summarize open PRs.

This is ops tooling for building Farrier. It is not a product module.

## Setup

```bash
export CURSOR_API_KEY=…   # https://cursor.com/dashboard/api
pnpm install
```

`prs` only needs `gh` auth. `spawn`, `follow-up`, and `status` need
`CURSOR_API_KEY`.

Open PR summaries include merge-conflict state (`merge:conflict` / `merge:ok`).

The package also exports library entrypoints (`summarizeOpenPrs`, `spawnImplementer`,
etc.) for `@farrier/fleet`.

## Commands

```bash
# Spawn an implementer (defaults to env evandelacruz/farrier, auto-PR on)
pnpm --filter @farrier/conductor spawn \
  --ids BKUP-002 \
  --name "Tenant ledgers" \
  -- "Implement BKUP-002 per docs/functional-requirements.md …"

# Follow up on an existing agent (same conversation / workspace)
pnpm --filter @farrier/conductor follow-up --agent bc-… -- "Fix unresolved review comments"

# List recent cloud agents (SDK source)
pnpm --filter @farrier/conductor status

# Open PRs + unresolved comment counts (via gh)
pnpm --filter @farrier/conductor prs
```

Prompt text may be passed as trailing args or on stdin.

## Defaults

| | |
|---|---|
| Environment | `evandelacruz/farrier` (named Cursor cloud env; includes this repo) |
| Model | `composer-2.5` (override with `--model` or `CURSOR_MODEL`) |
| Auto-PR | on (`--no-pr` to disable) |
| PR state | ready for review (not draft) — enforced by the conductor skill / implementer brief; mark with `gh pr ready` if a draft appears |
| Starting ref | env’s repo checkout (override with `--ref` only when using `--repo`) |

## Conductor skill

Invoke `/farrier-conductor` (or ask for a conductor pass). The skill lives at
`.cursor/skills/farrier-conductor/`.

Each pass watches open PRs for blocking review comments and also re-reads
**`APPROVED` but unmerged** PRs for nits and documentation asks worth doing
before Evan merges. `prs` groups PRs by which of those two paths they need;
the skill holds the triage rules and the "never merge" policy.
