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

There is no `gh` binary in a Claude Code session. Use `curl` against the REST
API, falling back to the MCP tools only where REST cannot serve.
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

**The GraphQL budget is the scarce one, by a wide margin.** The two buckets are
separate and sized differently — REST gets 15000/hr, GraphQL 5000 *points*/hr,
where one query costs points by how many nodes it returns rather than one per
call. Measured mid-session, on an account doing ordinary conductor and review
work:

```
core       14952/15000   used 48
graphql        0/5000    used 5000   ← exhausted
```

REST was untouched and GraphQL was gone. A pass that dies on rate limits has
almost certainly spent GraphQL, so the fix is never "make fewer calls" in
general — it is "make the GraphQL ones REST."

Read the buckets with `GET /rate_limit`, which is free and does not count
against either. Do it before blaming anything else for a failing pass:

```bash
curl -sS -H "Authorization: Bearer $GITHUB_TOKEN" \
  https://api.github.com/rate_limit | python3 -m json.tool
```

The 15000 REST ceiling is a GitHub App installation number. A PAT gets 5000.
Either way GraphQL is still the scarcer bucket — it only changes the headroom.

**Identifying a GraphQL tool.** The MCP server does not say which of its tools
are GraphQL, and the names give nothing away. Two tells:

- **The `search` bucket stays at 30/30 while `search_pull_requests` succeeds.**
  A REST search would have decremented it, so the call went to GraphQL.
- **The tool returns GraphQL node IDs** — `PRRT_…`, `PRRC_…`. REST returns
  integers.

Confirm by calling the REST equivalent with `curl` and seeing whether it 200s.
That last step matters. This skill previously asserted that check runs and
combined status were 403 for the session token; a retest showed both return
`200`, and the wrong claim had been routing the most frequent call in the pass
through GraphQL for no reason.

What is genuinely blocked is anything **not scoped to a repository**. The
session proxy answers `/search/issues` with `403 This GitHub API path is not
available: sessions are bound to their configured repositories.` That is the
rule to reason from — `/repos/{o}/{r}/...` works, cross-repo and global
endpoints do not — rather than a memorized list. Re-test before inheriting
any of it.

Four MCP tools reach GraphQL. Three have REST equivalents and must use them:

| Tool | Instead |
|---|---|
| `search_pull_requests` | **Never use it.** `GET /pulls?state=open` and filter client-side. `/search/issues` is blocked by the session proxy (not repo-scoped), so the MCP tool can only be GraphQL. |
| `pull_request_read` → `get_review_comments` | `GET /repos/{o}/{r}/pulls/{n}/comments` — every field except `isResolved` and the `PRRT_` thread id. Use the MCP tool **only** when about to resolve a thread. |
| `pull_request_read` → `get_check_runs` | `GET /repos/{o}/{r}/commits/{sha}/check-runs` — returns `200` with the full run list. An earlier version of this skill claimed it 403s; it does not. |
| `resolve_review_thread` | No REST equivalent exists anywhere in GitHub's API. Irreducible. |

Everything else about a review is REST and should be: creating a review,
submitting it with `COMMENT` / `APPROVE` / `REQUEST_CHANGES`, inline comments,
replies (`POST /pulls/{n}/comments/{id}/replies`), labels, and every list or
get.

Calls, per PR (`curl` against `api.github.com`, or the equivalent MCP tool):

| Field | Source |
|---|---|
| number, title, url, headRefName, headSha, isDraft, labels, issueComments | `GET /repos/{o}/{r}/pulls?state=open` |
| mergeable, mergeStateStatus, hasMergeConflict | `GET /repos/{o}/{r}/pulls/{n}` |
| reviewedShas | `GET /repos/{o}/{r}/pulls/{n}/reviews` |
| checksOk | `GET /repos/{o}/{r}/commits/{sha}/check-runs` |

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
- **`checksOk` comes from check runs, over REST.**

  ```bash
  curl -sS -H "Authorization: Bearer $GITHUB_TOKEN" \
    -H "Accept: application/vnd.github+json" \
    "https://api.github.com/repos/{o}/{r}/commits/{sha}/check-runs"
  ```

  This skill previously said the endpoint returns `403 Resource not accessible
  by integration` and that the `get_check_runs` MCP tool was the only way. That
  was wrong — retested and it returns `200` with the full run list — and it was
  expensive to believe, because the MCP tool is GraphQL and this is the one
  field gathered for every open PR on every pass. Use `curl`.

  Use `/commits/{sha}/check-runs`, never `/commits/{sha}/status`. GitHub Actions
  writes **check runs**, not the legacy commit statuses `/status` reports, so
  `/status` returns `200` with an empty `statuses` array even on a PR whose CI
  is genuinely red — the failure mode is silent, and it makes every PR look
  like it has no CI so a red build never queues a fixer.

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
commit is unreviewed is still reported as skipped (`reviews-disabled`), so you
can see the webhook keeping up (or not). When the webhook misses, §3b fires the
reviewer — that is the self-heal. Do not put reviews back into the plan.

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
     -H "Authorization: Bearer <the key for item.role — see the table below>" \
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

   | `role` | Routine | Key |
   |---|---|---|
   | `implementer` | the Farrier implementer routine (opus) | `FARRIER_IMPLEMENTER_FIRE_KEY` |
   | `reviewer` | the Farrier code-review routine (sonnet) — webhook-driven; the conductor should not normally fire it from the plan | `FARRIER_REVIEWER_KEY` |

   If the routine for a role does not exist, **stop and say so.** Do not fall
   back to the other role, and do not do the work yourself.

   **Each routine has its own token, and they are not interchangeable.** The
   implementer key does not fire the reviewer routine — confirmed, not assumed.
   Firing a reviewer with the implementer key fails; there is no fallback.

   If a role's key is unset, run `env | grep -i -E 'fire|key'` before concluding
   it is missing — the two names above do not share a suffix, so a single
   pattern will not find both. Environment changes only reach **new** sessions,
   so a key added mid-session is invisible until the next one; say so rather
   than reporting the key as absent.

3. If firing fails, **release the lock you just took** before moving on.
   A lock with no agent behind it parks the PR until a human clears it.
   Delete **`item.lockLabel`**, not a hardcoded name — a `pr-review` lock is
   `conductor:reviewing`, and deleting `conductor:working` leaves the PR
   stranded as "review in flight" forever:

   ```bash
   curl -sS -X DELETE -H "Accept: application/vnd.github+json" \
     -H "Authorization: Bearer $GITHUB_TOKEN" \
     "https://api.github.com/repos/evandelacruz/farrier/issues/<n>/labels/<lockLabel>"
   ```

## 3b. Fire a review the webhook did not

Reviews normally arrive on their own: pushing fires the reviewer Routine, which
is why the plan runs with `--no-reviews`. When that path fails, though, nothing
retries it — and **the only thing that re-triggers a review is another push**,
which will never come, because the PR is waiting for the review.

The failure is not rare. A reviewer that hits a GitHub rate limit is told to
delete its pending review and exit, and one that leaves an orphaned pending
review behind blocks every reviewer after it. Both leave a PR sitting with no
verdict and nothing coming. That is the stall this repo has hit before.

So after firing the plan, fire a reviewer for any PR where both of these hold:

- no review at the head SHA **carries a verdict**, and
- the PR carries no `conductor:reviewing` label.

Both are facts, not elapsed-time guesses: nothing has judged the current code,
and nothing is judging it. **There is deliberately no delay before firing.** If
the webhook's reviewer is claiming the PR at the same moment, the loser hits
*"User can only have one pending review per pull request"* and exits without
writing anything — GitHub enforces that mutex server-side, which is a better
guard than any interval would be, and it costs one session start.

**Do not add `conductor:reviewing` before firing.** The reviewer takes that
lock itself, and it reads the label on entry as *another reviewer already has
this PR* — so a conductor that pre-sets it makes the reviewer exit immediately,
having done nothing, and leaves the label behind. The PR then looks like a
review is in flight when none ever started, which is exactly the stall §3b
exists to clear.

This is the opposite of §3's implementer flow, and the asymmetry is easy to get
wrong: the conductor takes `conductor:working` **before** firing an implementer,
but never takes `conductor:reviewing` for a reviewer. Fire the reviewer against
a PR with no reviewing lock on it and let the routine claim its own.

If a PR is already carrying a stale `conductor:reviewing` — a reviewer that died,
or one a conductor stranded this way — delete it first, then fire. A lock with
no session behind it never expires on its own.

**"Carries a verdict" is the load-bearing part, and a review existing is not
enough.** A reviewer posts its inline findings as a review with an empty body,
then writes the summary and sets the verdict label. Kill it between those two
steps — a rate limit is the usual way — and the PR is left with one or more
reviews at the head SHA, every one of them empty, and no verdict anywhere.

That state passes a "has it been reviewed" test and fails the only test that
matters. So check the review **bodies**, not just their `commit_id`. A review
whose body is empty is an inline-comment batch, not a judgement:

```bash
curl -sS -H "Authorization: Bearer $GITHUB_TOKEN" \
  "https://api.github.com/repos/evandelacruz/farrier/pulls/<n>/reviews" \
  | python3 -c "
import json,sys
for r in json.load(sys.stdin):
    print(r['state'], r['commit_id'][:8], 'body' if (r['body'] or '').strip() else 'EMPTY')"
```

Then tell the reviewer you fire what it is walking into, so it reads the dead
reviewer's inline comments instead of starting from nothing — those findings
cost a full run and are usually still valid.

Do not read a verdict label as "this has been reviewed" either. It describes
whichever commit it was written against, so a PR that was pushed to and never
re-reviewed still shows the old one. The head SHA is what settles it.

Resolve the reviewer routine id with `list_triggers` (do not hardcode it).
Firing it directly is the same thing a push does, minus the push:

```bash
curl -sS -X POST \
  "https://api.anthropic.com/v1/claude_code/routines/<reviewer routine id>/fire" \
  -H "Authorization: Bearer $FARRIER_REVIEWER_KEY" \
  -H "anthropic-beta: experimental-cc-routine-2026-04-01" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  --data @<payload>.json      # {"text": "Review PR #<n> in evandelacruz/farrier."}
```

**It needs its own token.** Routine tokens only fire the Routine that issued
them, so `FARRIER_IMPLEMENTER_FIRE_KEY` returns *"Token is not authorized for
this routine"* here. If `FARRIER_REVIEWER_KEY` is missing from the environment,
**report these PRs in the summary instead of firing** — naming them is most of
the value, and a human can flip the PR to draft and back to trigger a review by
hand.

**If a fired review produces nothing, suspect an orphaned pending review.** It
is invisible to `GET /pulls/{n}/reviews`, and every reviewer is told to exit
quietly when it finds one, so the symptom is silence. Confirm by attempting
`pull_request_review_write` with method `create`: a failure saying *"User can
only have one pending review per pull request"* is the orphan. Clear it with
`delete_pending`.

Do **not** probe speculatively. If the `create` succeeds there was no orphan,
and you have just made a pending review you must remember to delete — the exact
thing you were hunting.

## 4. Report

One compact summary: what was fired (role, model, target), what was skipped and
why, §3b backfills, and any slot that could not be filled. Link the PRs. Then
stop — do not poll for the workers, they notify on completion.

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
  fires a fixer, the fixer pushes, the push webhook fires another review. When
  the webhook misses, §3b fires it. **It runs unbounded** — there is no round
  cap and nothing parks a PR for having been round-tripped too many times. A PR
  that is not converging keeps drawing a fixer every pass until Evan steps in.
- **A fixer cannot answer a question only Evan can answer.** When a review's
  remaining blocker is a design decision — a doc conflict, a spec
  reinterpretation — firing a fixer at it spends an agent that has nothing to
  change, and the next review re-raises it. Surface the question to Evan
  instead, and once he answers, record it **on the review thread** and resolve
  the thread. An answer posted only as a PR comment does not clear the blocker:
  reviews key on the open thread, so the PR keeps drawing "needs Evan's call"
  verdicts until the thread itself is closed.
