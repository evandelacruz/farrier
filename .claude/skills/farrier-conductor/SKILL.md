---
name: farrier-conductor
description: Run a Farrier build conductor pass — plan n agent assignments across open PRs and backlog work, then fire the implementer/reviewer Routines that spawn them. Use when asked to "run the conductor", "spawn n agents", "kick off a fleet pass", or to review/fix open PRs in bulk.
---

# Farrier build conductor

Plan `n` agent assignments and fire one Routine per assignment. Each firing
spawns a fresh Claude Code cloud session, visible on desktop and phone.

**You are the conductor, not a worker.** Do not fix PRs, review code, or
implement requirements yourself. Plan, fire, report, stop.

## Invariants

- **Never merge a PR.** Evan merges. Nothing here merges, ever.
- **Never fire without a plan.** The plan decides what runs; you do not
  hand-pick assignments.
- **`n` is required.** Ask if it was not given.
- One agent per PR. The lock labels exist to keep that true.

## 1. Gather open PRs

There is no `gh` binary in a Claude Code session — use the GitHub MCP tools.
Build a JSON array of `PrCommentSummary` objects, one per open PR:

```jsonc
{
  "number": 75,
  "title": "…",
  "url": "https://github.com/evandelacruz/farrier/pull/75",
  "headRefName": "cursor/auth-008-…",
  "headSha": "237b582…",          // head.sha
  "isDraft": false,
  "reviewDecision": null,          // APPROVED | CHANGES_REQUESTED | null
  "mergeable": "MERGEABLE",
  "mergeStateStatus": "CLEAN",
  "hasMergeConflict": false,
  "unresolvedReviewThreads": 0,
  "issueComments": 0,
  "checksOk": null,                // true | false | null (null = no CI, or still running)
  "labels": [],
  "reviewedShas": []             // real reviews only — see the filter below
}
```

**Use REST, not GraphQL.** The session's egress proxy serves only a pinned set
of GraphQL operations and rejects anything else with *"This GraphQL query is not
enabled for this session."* That also rules out `summarizeOpenPrs()` from
`@farrier/conductor`, which is built on `gh api graphql` — it works on a laptop,
not here.

Calls, per PR (`curl` against `api.github.com`, or the equivalent MCP tool):

| Field | Source |
|---|---|
| number, title, url, headRefName, headSha, isDraft, labels, issueComments | `GET /repos/{o}/{r}/pulls?state=open` |
| mergeable, mergeStateStatus, hasMergeConflict | `GET /repos/{o}/{r}/pulls/{n}` |
| reviewedShas | `GET /repos/{o}/{r}/pulls/{n}/reviews` |
| checksOk | `pull_request_read` with `method: "get_check_runs"` — **not** curl |

Normalization that is easy to get wrong:

- **`mergeable_state` comes back lowercase.** Uppercase it. `dirty` means a
  merge conflict → `hasMergeConflict: true`, `mergeStateStatus: "DIRTY"`.
- **Set `reviewDecision: null`.** REST has no such field, and an agent review
  can never produce one anyway — the verdict labels carry it, and the routing
  table reads them via `effectiveReviewDecision()`.
- **Set `unresolvedReviewThreads: 0`.** Thread resolution is GraphQL-only, so
  it is unavailable here. This is why the verdict label matters: without it,
  nothing tells the conductor a PR has blocking findings.
- **`reviewedShas` excludes `PENDING` reviews.** A pending review is an unsent
  draft — counting it would mask the PR out of the review queue.
- **`reviewedShas` also excludes reviews with an empty body.** When a fixer
  replies to a review thread, GitHub wraps those replies in a review object of
  its own — `state: "COMMENTED"`, `body: ""`, `commit_id` set to the head the
  fixer just pushed. It is the fixer talking, not a reviewer, but it is
  indistinguishable from a real review by state and SHA alone.

  Counting it is the worst kind of wrong, because it strands the PR in a state
  no later pass can escape: the new head looks reviewed, so nothing queues a
  review, while the stale `conductor:changes-requested` label still says the PR
  has blocking findings. Too reviewed to queue, too rejected to merge — and
  nothing in the plan output looks off.

  Filter on the body. The reviewer routine always submits with a comment, so a
  genuine review has one; a replies-only wrapper never does:

  ```python
  reviewedShas = sorted({
      r["commit_id"] for r in reviews
      if r.get("state") != "PENDING"
      and (r.get("body") or "").strip()
      and r.get("commit_id")
  })
  ```
- **`checksOk` comes from check runs, and `curl` cannot read them.** Both
  `GET /commits/{sha}/status` and `GET /commits/{sha}/check-runs` return
  `403 Resource not accessible by integration` for the session's token. Use the
  MCP tool instead — `pull_request_read` with `method: "get_check_runs"` —
  which works. Getting this wrong is silent: a 403 parsed as "no checks" makes
  every PR look like it has no CI, and a red build never queues a fixer.

  `/status` is also the wrong endpoint on its own terms. GitHub Actions writes
  **check runs**, not the legacy commit statuses `/status` reports, so that
  call can return an empty result on a PR whose CI is genuinely red.

  Map the response like this:

  | Check run | `checksOk` |
  |---|---|
  | `total_count: 0` | `null` — no CI configured for that commit |
  | every run `status: "completed"` and `conclusion: "success"` (or `"neutral"` / `"skipped"`) | `true` |
  | any run `completed` with `failure` / `timed_out` / `cancelled` / `action_required` | `false` |
  | any run **not** `completed` (`queued`, `in_progress`) | `null` |

- **In-progress CI is `null`, never `false`.** A run that has not finished has
  not failed. Mapping it to `false` fires a fixer at a healthy PR every time a
  pass lands in the minutes after a push — which is exactly when passes happen,
  since a push is what triggers both the review and the next pass.

Write the array to a scratch file, e.g. `<scratch>/prs.json`.

## 2. Plan

**Always plan from a fresh `origin/main` worktree, never from the working tree.**

Fleet reads two different worlds. PR routing is live from the GitHub API, so it
is current no matter where you stand. The backlog is not: `docs/status.json` and
`docs/functional-requirements.md` are read **off disk**, so whatever branch happens to
be checked out decides what work exists. Any branch cut before the last few
merges still says `open` for requirements that have since shipped — and the
plan will confidently assign an agent to build something that is already on
main.

The failure is silent and costs a whole agent. There is nothing in the plan
output that looks wrong.

```bash
pnpm --filter @farrier/conductor build && pnpm --filter @farrier/fleet build

git fetch origin main
git worktree remove --force <scratch>/main-wt 2>/dev/null
git worktree prune
git worktree add --detach <scratch>/main-wt origin/main

node tools/fleet/dist/cli.js plan \
  --repo-root <scratch>/main-wt \
  --n <n> \
  --prs <scratch>/prs.json \
  --out <scratch>/run \
  --no-reviews
```

**Point `--repo-root` at the worktree; do not `cd` into it.** `--repo-root` is
what `docs/status.json` and `docs/functional-requirements.md` are resolved against, and
those two reads are the whole staleness problem. Staying in the working tree
keeps `node_modules` and the built CLI where they already are.

Clearing a previous `main-wt` takes `git worktree remove` plus `prune`, not
`rm -rf` — deleting the directory leaves metadata under `.git/worktrees/` that
makes the next `worktree add` fail. Remove the worktree when the pass is done:
`git worktree remove --force <scratch>/main-wt`.

**Sanity check before firing.** For every `implement` assignment, confirm the ID
really is unshipped on main:

```bash
git show origin/main:docs/status.json | grep -o '"<ID>": *[^,}]*'
```

`landed` means the plan is stale — you are not on main, or the fetch did not
take. Re-do the worktree rather than firing.

**`--no-reviews` is required in this repo.** The reviewer Routine is bound to a
`Pull request: Commits pushed` webhook, so every push already gets reviewed.
Dropping the flag would review each push twice and spend half the batch on work
that was already coming for free.

Passing it does not mean reviews stop mattering to the plan: a PR whose head
commit is unreviewed is still reported as skipped, so you can see the webhook
keeping up (or not).

Print the plan output verbatim. It names every assignment **and a reason for
every PR it passed over** — that is the part Evan reads.

Stop and ask if the plan reports `ASK_PAUSE`.

## 3. Fire

Read `<scratch>/run/dispatch.json`. For each item, in order:

1. **Take the lock** (skip when `lockLabel` is `null`, i.e. new implement work).
   Use the **additive** label endpoint — `POST /issues/{n}/labels` — rather
   than `issue_write`, which replaces the whole label set and would silently
   drop the verdict label the routing table depends on:

   ```bash
   curl -sS -X POST \
     -H "Accept: application/vnd.github+json" \
     -H "Authorization: Bearer $GITHUB_TOKEN" \
     "https://api.github.com/repos/evandelacruz/farrier/issues/<prNumber>/labels" \
     -d '{"labels":["<lockLabel>"]}'
   ```

   Create the label first if the repo does not have it yet.

2. **Fire the Routine** matching `item.role`, over the routine's HTTP endpoint:

   ```bash
   curl -sS -X POST \
     "https://api.anthropic.com/v1/claude_code/routines/<routine id>/fire" \
     -H "Authorization: Bearer $FARRIER_IMPLEMENTER_FIRE_KEY" \
     -H "anthropic-beta: experimental-cc-routine-2026-04-01" \
     -H "anthropic-version: 2023-06-01" \
     -H "Content-Type: application/json" \
     --data @<payload>.json      # {"text": "<item.text>"}
   ```

   Build the JSON body with a tool (`json.dump`), never by string-concatenating
   the brief into a shell heredoc — the text contains backticks, quotes, and
   `$` and will not survive it.

   A success returns `claude_code_session_url`. **Report that URL** — it is how
   Evan watches the run.

   **Do not use the `fire_trigger` MCP tool here.** It only fires routines the
   calling agent created itself; these were created through the API, and it
   fails with *"Agents can only fire routines they created."* The HTTP endpoint
   has no such restriction — it authenticates with the routine's own token.

   Resolve the routine id by name with `list_triggers` rather than hardcoding
   it — Evan edits these:

   | `role` | Routine |
   |---|---|
   | `implementer` | the Farrier implementer routine (opus) |
   | `reviewer` | the Farrier code-review routine (sonnet) — webhook-driven; the conductor should not normally fire it |

   If the routine for a role does not exist, **stop and say so.** Do not fall
   back to the other role, and do not do the work yourself.

   Each routine has its **own** token, scoped to firing that one routine. If
   `FARRIER_IMPLEMENTER_FIRE_KEY` is unset, check `env | grep -i fire` for a
   differently-named variable before concluding it is missing — and remember
   environment changes only reach **new** sessions, so a key added mid-session
   is not visible until the next one.

3. If firing fails, **release the lock you just took** before moving on.
   A lock with no agent behind it parks the PR until a human clears it.

   ```bash
   curl -sS -X DELETE -H "Accept: application/vnd.github+json" \
     "https://api.github.com/repos/evandelacruz/farrier/issues/<n>/labels/conductor:working"
   ```

## 4. Report

One compact summary: what was fired (role, model, target), what was skipped and
why, and any slot that could not be filled. Link the PRs. Then stop — do not
poll for the workers, they notify on completion.

## Notes

- The conductor holds no state, but it does not read everything from one place.
  PRs come live from the GitHub API; the backlog comes off disk. Running a pass
  twice in a row is safe as long as the locks are honest **and** the plan ran
  against a fresh `origin/main` worktree (§2) — otherwise the PR half is current
  and the backlog half is however stale the checked-out branch is.
- A stale lock (worker died before dropping its label) parks that PR. Clearing
  it is a manual call — remove the label and the next pass picks the PR up.
- Open PRs always win slots over new backlog work. A growing PR queue starving
  new feature work is intended, not a bug.
- **Verdict labels carry the review state.** Agent reviews post under the repo
  owner's identity and GitHub forbids approving your own PR, so
  `reviewDecision` is always `null` and the reviewer records
  `conductor:approved` / `conductor:changes-requested` instead. The conductor
  reads whichever exists, preferring a real GitHub decision — so if reviews
  later come from a separate identity (a human, or another vendor's bot), that
  takes over automatically and the labels stop mattering. Nothing to change.
- Review and fix form a closed loop: a review requests changes, the conductor
  fires a fixer, the fixer pushes, the push webhook fires another review. That
  is the intent, and **it runs unbounded** — there is no round cap and nothing
  parks a PR for having been round-tripped too many times. A PR that is not
  converging keeps drawing a fixer every pass until Evan steps in.
