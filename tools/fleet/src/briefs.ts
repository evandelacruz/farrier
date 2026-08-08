/**
 * Deliberately does not ask for a full-suite run.
 *
 * CI runs `pnpm build` and the whole suite on every PR targeting main, so an
 * agent repeating it is paying ~97s of wall clock — most of it recompiling
 * packages it never touched — for an answer that is coming anyway.
 *
 * What it does still ask for is the *scoped* run, because that is the one CI
 * cannot make cheap: a break the agent can see in-session costs seconds to
 * fix, and the same break found by CI costs a red build, a conductor pass,
 * and a fresh implementer session. Writing tests is untouched — that is what
 * CI executes.
 */
const QUALITY_BLOCK = `
## Quality bar
- Reasonable test coverage for behavior you add or change.
- Run the tests **for the packages you touched** before calling the PR ready —
  e.g. \`go test ./internal/core/backup/...\`. Fix failures you introduced.
- Do **not** run the full \`go test ./...\`. CI runs the whole suite, plus the
  build, on every PR into main and will report failures on the PR.
  Repeating it here mostly rebuilds packages you did not touch.
- **Never open a PR as a draft.** REST cannot take one back out of draft, and
  the GraphQL mutation that can is on a budget the fleet routinely exhausts —
  so a draft can strand finished work permanently. See "When to push".
- If the solution is not simple, step back and reconsider the approach. Be
  willing to change approach when it makes the future better. Do not force
  round pegs into square holes.
`.trim();

/**
 * Serial slicing. Several small PRs under one requirement ID is the expected
 * shape, not a fallback — a huge diff usually means an unsliced requirement
 * rather than a big one.
 *
 * The rule this relaxes ("no thin stubs") exists because agents shipped the
 * minimum to unlock a dependent and marked the ID done. The discriminator kept
 * here is that a legitimate slice is **declared before implementing and
 * complete for what it covers**, where a stub is discovered by running out of
 * steam and leaves holes behind. `partial` in docs/status.json is already
 * excluded from the landed set, so a sliced ID is re-queued automatically on a
 * later pass, carrying its `remaining` note.
 *
 * The prerequisite carve-out is the one sanctioned layer cut. PR #98 halted on
 * AUTH-014 because the PM portal did not exist; with the frontend stack now
 * settled in §5, the correct move is to build the prerequisite as its own slice
 * rather than stop. Halting belongs to missing *decisions*, not missing *code*.
 */
const SLICING_BLOCK = `
## If the ID is too large for one PR

**One ID may take as many PRs as it honestly needs.** Slice whenever the whole
would be a PR no reviewer can hold in their head. A huge diff is not evidence of
a big requirement; it is usually evidence of an unsliced one.

**Split by capability, never by layer.** A capability slice is a vertical
cut — schema, service, API, and tests for one coherent behavior — that leaves
\`main\` working and can be reviewed on its own merits. A layer cut ("all the
migrations now, the API next") is not a slice: it lands dead code, cannot be
reviewed for correctness alone, and is the thing this rule forbids.

**One exception: a prerequisite that is decided but unbuilt.** If the ID needs
something \`docs/spec.md\` or \`docs/tech-spec.md\` already settles — a package, a surface, a shared
client — and nobody has built it yet, that prerequisite is a legitimate slice
even though it is a layer. Land it on its own, with its own tests, and let the
feature follow in a later pass. The alternative is one PR carrying a foundation
and a feature together, which nobody can review as either. This is the only
sanctioned layer cut, and it is sanctioned because the layer is independently
useful, not because it is convenient.

If you slice:

1. Decide the slice boundaries **before implementing**, and list every slice
   (this one and the ones deferred) in the PR description.
2. Implement this slice's full MUST. No stubs, no TODOs standing in for the
   deferred slices, no half-built abstractions waiting to be filled in.
3. Record the ID in docs/status.json as
   \`{"state":"partial","remaining":"<what is left>"}\` — never \`"landed"\`.
   One line, concrete enough to act on. A later pass re-queues the ID and is
   handed that line verbatim, so it is the only thing carrying the slice
   boundary forward.

If you cannot state the deferred slices concretely, the ID is not sliceable —
implement it whole or halt and say why.

**"Decided but not built" is not a stop condition.** If the docs settle
the thing you need and it simply does not exist in the tree yet, build it — as
its own slice, on the terms above. Halt only when the decision itself is
missing. Halting on absence rather than on ambiguity burns a session
rediscovering that the work has not been done.
`.trim();

const STOP_CONDITIONS = `
## Stop conditions
If you hit an open architecture, stack, or design question — halt and print why.
Do not invent stack or dependencies. Every decision in docs/ is negotiable with
Evan and with nobody else: if implementation shows a documented decision is
wrong, state the problem and a proposed alternative and stop. Do not silently
deviate. Do not weaken snapshot verification, the state model, or the identity
model without flagging it prominently in the PR.

`.trim();

/** PR fix/polish agents must merge latest main before addressing review feedback. */
export const SYNC_WITH_MAIN = `
## First step — sync with main locally (mandatory, do not push yet)

PRs are reviewed against \`main\`. Stale branches cause "missing code already
on main" feedback. Merge main **locally first** — before reading review
threads or coding:

\`\`\`bash
git fetch origin main
git merge origin/main
\`\`\`

- Merge \`origin/main\` into your **current branch** (feature/PR branch). Use merge,
  not rebase, unless merge fails and you document why you switched.
- If there are merge conflicts: resolve them completely before review fixes.
- **Do not push after the merge.** Push triggers automated code review; wait until
  review feedback is addressed and tests pass (see "When to push" below).

Do not skip the merge step. Reviewers treat missing main commits as a blocker.
`.trim();

/**
 * New implementers sync to latest main, then claim the ID by opening the PR
 * ready for review — never as a draft.
 *
 * Draft was the obvious claiming vehicle (a draft is not reviewed, so it costs
 * nothing to open one early) and it is a trap. Draft state is create-time-only
 * in REST: `POST /pulls` accepts `draft`, `PATCH /pulls/{n}` silently ignores
 * it — 200, no error, PR unchanged. The only way back out of draft is the
 * GraphQL `markPullRequestReadyForReview` mutation, and the GraphQL budget is
 * 5000 points/hr shared across every session on the account, routinely
 * exhausted by the fleet. An agent that drafts, works, and then cannot flip
 * ready leaves its finished work invisible: a draft is never reviewed and
 * never lands.
 *
 * Opening ready-from-start removes the transition entirely, so no part of the
 * lifecycle depends on GraphQL. Review suppression during the work is the
 * `conductor:working` lock's job instead of draft state's.
 */
const SYNC_WITH_MAIN_IMPLEMENT = `
## First step — start from latest main (mandatory)

\`\`\`bash
git fetch origin main
git checkout -B <branch> origin/main
\`\`\`

The **Branch** section at the end of this assignment names the branch to use.
Never reuse or copy another PR's branch name.

- If you were already placed on a working branch, stay on it and bring main in
  instead of branching again:

\`\`\`bash
git merge origin/main
\`\`\`

- Prefer merge over rebase unless merge fails and you document why you switched.
- Then claim the ID immediately, **before writing any implementation code** —
  see "Second step".

## Second step — claim the ID by opening the PR (mandatory, before coding)

Write \`.github/branch-notes/<slug>.md\`, commit it, push the branch, and open
the PR with the requirement ID in the title. Do this before you implement
anything.

\`\`\`bash
git push -u origin HEAD          # branch note only

# Open it READY, never as a draft. Then take the lock in the same breath.
curl -sS -X POST -H "Authorization: Bearer \$GITHUB_TOKEN" \\
  -H "Accept: application/vnd.github+json" \\
  https://api.github.com/repos/evandelacruz/farrier/pulls \\
  -d '{"title":"<ID>: <what you are building>","head":"<branch>","base":"main","body":"...","draft":false}'

curl -sS -X POST -H "Authorization: Bearer \$GITHUB_TOKEN" \\
  -H "Accept: application/vnd.github+json" \\
  https://api.github.com/repos/evandelacruz/farrier/issues/<n>/labels \\
  -d '{"labels":["conductor:working"]}'
\`\`\`

The open PR is the only signal that this ID is claimed. Until it exists, a
conductor pass has no way to see you working — the plan reads open PRs, not
running sessions — so a second agent can be assigned the same ID and build the
same thing in parallel. That has happened repeatedly and each occurrence costs
one of the two implementations entirely.

**Never open it as a draft.** Draft is create-time-only in REST: \`PATCH\` on a
pull request silently ignores \`draft\` — 200, no error, nothing changes — and
the only way out of draft is a GraphQL mutation. The GraphQL budget is shared
across every agent on this account and is routinely exhausted, so an agent that
drafts and then cannot flip ready leaves finished work invisible forever. Open
ready and there is no transition to lose.

\`conductor:working\` is what keeps reviewers off the PR while you build. Drop it
at the very end (see "When to push"), and that drop is the handoff.
`.trim();

const WHEN_TO_PUSH_IMPLEMENT = `
## When to push

You have already pushed once, to open the PR that claims the ID ("Second step").
That PR is already ready for review — there is no draft to flip, and no
GraphQL call anywhere in this lifecycle.

Push again **once**, when the slice is complete and its tests pass. Do not push
after a mid-work merge from main on its own. Every push while
\`conductor:working\` is set is yours; the label is what tells reviewers the PR
is still being built.

**The handoff is dropping \`conductor:working\` after your final push** — not any
state change on the PR itself:

\`\`\`bash
git push origin HEAD

curl -sS -X DELETE -H "Authorization: Bearer \$GITHUB_TOKEN" \\
  -H "Accept: application/vnd.github+json" \\
  https://api.github.com/repos/evandelacruz/farrier/issues/<n>/labels/conductor:working
\`\`\`

Drop the label on **every** exit path, including when you halt without
finishing. A lock with no session behind it parks the PR until a human clears
it by hand.

An automated reviewer reads the PR and posts findings you will get a chance to
fix. Reviewing your own diff first is **not** your job — push when the work is
done and let the review happen.
`.trim();

const WHEN_TO_PUSH_FIX = `
## When to push

Push **once**, after main is merged locally, review feedback is addressed, and
tests pass. Do not push after the merge alone — each push triggers automated
code review.

\`\`\`bash
git push origin HEAD
\`\`\`

This PR already exists and is already ready for review. Leave it that way —
never flip it to draft, which would stop every further review on it.
`.trim();

export function buildImplementerPrompt(
  ids: string[],
  options: {
    chunkTitle?: string;
    chunkKey?: string;
    /** True when fleet folded conflict-skipped chunks into this agent. */
    combinedDueToConflict?: boolean;
    /** `remaining` notes for any assigned ID that is `partial` in status.json. */
    remaining?: Array<{ id: string; remaining: string }>;
  } = {},
): string {
  // A completion pass is told what is left rather than re-deriving it. The
  // note came from the agent that drew the slice boundary, which knew it for
  // free; without it this agent would diff spec against code to guess.
  const remainingNote =
    options.remaining && options.remaining.length > 0
      ? [
          "",
          "## Remaining on this ID (it is `partial`)",
          "Finish exactly this. It is the slice boundary recorded by the pass that",
          "shipped the earlier slice — not a summary of the whole requirement.",
          ...options.remaining.map((r) => `- **${r.id}**: ${r.remaining}`),
        ]
      : [];

  const combinedNote = options.combinedDueToConflict
    ? [
        "",
        "These IDs were **combined into one agent** because they are an **explicit",
        "co-ship pair** (listed partners only — not arbitrary same-domain neighbors)",
        "and one was conflict-skipped; fold absorbs the partner instead of abandoning it.",
        "Implement them **in listed order** (spec / dependency order), full MUST each,",
        "**one PR**.",
      ]
    : [];

  return [
    "You are an implementer on the Farrier repo (github.com/evandelacruz/farrier).",
    "",
    SYNC_WITH_MAIN_IMPLEMENT,
    "",
    "## Requirements",
    options.chunkTitle
      ? `Work chunk: ${options.chunkTitle}${options.chunkKey ? ` (${options.chunkKey})` : ""}`
      : "Work chunk: full scope of the listed requirement ID(s).",
    "Implement the **complete** MUST for each ID from docs/functional-requirements.md — not a",
    "thin stub and not the bare minimum to unlock a dependent. If the ID is genuinely",
    "too large for one reviewable PR, see \"If the ID is too large for one PR\" below —",
    "that is the only sanctioned way to deliver less than the whole ID.",
    "If the ID is `partial` in docs/status.json, its `remaining` line is quoted",
    "below — finish exactly that, and do not leave new stubs to unlock later work.",
    ...combinedNote,
    ...remainingNote,
    "",
    "IDs:",
    ...ids.map((id) => `- ${id}`),
    "",
    "Read before coding:",
    "1. CLAUDE.md (invariants, glossary, stack — do not invent tooling)",
    "2. **`docs/spec.md` — the decision record. A settled decision outranks your",
    "   judgement; raise it with Evan rather than deviating.**",
    "   It settles things CLAUDE.md never mentions: the state model, the identity",
    "   model, what the operator owns. A settled decision is not yours to re-open.",
    "3. The cited sections of docs/functional-requirements.md — implement the full requirement text",
    "4. docs/functional-requirements.md § Dependency order for what must land first",
    "",
    "## Scope",
    "- One PR at a time for this chunk — never two in parallel. If the ID is large,",
    "  slice it serially per the section below and let later passes pick up the rest.",
    "  Several small PRs under one ID is the expected shape, not a fallback.",
    "- Use the branch named in the Branch section below; do not open a parallel one.",
    "- Reference the requirement IDs in commits and the PR body.",
    "- Follow existing package shape under internal/core.",
    "- Follow the package layout in docs/tech-spec.md. All logic in internal/core;",
    "  cmd/farrier and internal/api stay thin.",
    "- Prefer a complete, simple design for the chunk over a partial unlock.",
    "",
    SLICING_BLOCK,
    "",
    QUALITY_BLOCK,
    "",
    "",
    WHEN_TO_PUSH_IMPLEMENT,
    "",
    STOP_CONDITIONS,
    "",
    "## If unsure",
    "If chunk boundaries, dependency order, or architecture are unclear",
    "— **halt and print why**. Do not guess. Slicing is allowed only on the terms above;",
    "anything else that delivers less than the full MUST is a stub, not a slice.",
    "",
    "## Done means",
    "- The full MUST for each listed ID is met (schema, API, tests as needed)",
    "- Tests that exercise the behavior",
    "- PR opened **ready** before coding, with the IDs in the title or body,",
    "  and `conductor:working` dropped at the end — that drop is the handoff",
    "- docs/status.json flipped to `landed` for each ID — or",
    "  `{\"state\":\"partial\",\"remaining\":\"...\"}` if a deliberate slice remains",
  ].join("\n");
}

export function buildPrFixPrompt(pr: {
  number: number;
  url: string;
  title: string;
  reasons: string[];
}): string {
  return [
    `You are fixing open PR #${pr.number} on the Farrier repo.`,
    "",
    `PR: ${pr.url}`,
    `Title: ${pr.title}`,
    `Why this pass: ${pr.reasons.join("; ")}`,
    "",
    SYNC_WITH_MAIN,
    "",
    "## Task",
    "1. Merge main locally (above) — do not push yet.",
    "2. Address all requested changes and unresolved review threads.",
    "3. Resolve any remaining merge conflicts from the main sync.",
    "4. Run tests.",
    "5. Push once (see When to push).",
    "- Keep the same requirement scope unless a comment requires a halt.",
    "- Keep the PR ready for review (not draft); mark it ready if it is one.",
    "",
    QUALITY_BLOCK,
    "",
    "",
    WHEN_TO_PUSH_FIX,
    "",
    STOP_CONDITIONS,
  ].join("\n");
}

export function buildPrPolishPrompt(pr: {
  number: number;
  url: string;
  title: string;
  reasons: string[];
}): string {
  return [
    `You are polishing approved PR #${pr.number} before merge.`,
    "",
    `PR: ${pr.url}`,
    `Title: ${pr.title}`,
    `Why this pass: ${pr.reasons.join("; ")}`,
    "",
    SYNC_WITH_MAIN,
    "",
    "## Task",
    "1. Merge main locally (above) — do not push yet.",
    "2. Read all review bodies, issue comments, and review threads (resolved or not).",
    "3. Fix items worth doing now before merge:",
    "- Documentation nits and missing README / docs/status.json notes",
    "- Small clarity renames and comment wording",
    "- Test and assertion tighten-ups that reduce tech debt or simplify",
    "- Anything marked nit/suggestion/docs that should be done at merge",
    "4. Run tests.",
    "5. Push once (see When to push).",
    "",
    "Do NOT expand scope: no drive-by refactors, new features, or stack inventions.",
    "If a nit is actually an architecture question — halt and print why.",
    "- Keep the PR ready for review (not draft).",
    "",
    QUALITY_BLOCK,
    "",
    "",
    WHEN_TO_PUSH_FIX,
    "",
    STOP_CONDITIONS,
  ].join("\n");
}

/**
 * Assignment payload for the reviewer Routine — **not** a review standard.
 *
 * The Routine's own stored prompt owns how to review and how to post. Repeating
 * that here would give the session two overlapping sets of instructions to
 * reconcile, and the fired text is the weaker of the two places to keep it
 * (editing a brief means shipping a commit; editing the Routine does not).
 * So this carries only what the Routine cannot know: which PR, at which commit,
 * and the Farrier-specific invariants a general-purpose reviewer would not think to
 * check.
 */
export function buildPrReviewPrompt(pr: {
  number: number;
  url: string;
  title: string;
  headSha: string;
}): string {
  return [
    `Review PR #${pr.number} — ${pr.url}`,
    `Title: ${pr.title}`,
    `Head commit: ${pr.headSha} (review this commit; say so if the branch has moved)`,
    "",
    "Follow your standing review instructions. Do not push code and do not merge.",
    "",
    "## Farrier-specific priorities (in addition to your usual pass)",
    "",
    "Read `CLAUDE.md`, the sections of `docs/spec.md` and `docs/tech-spec.md` that",
    "touch the changed area, and the requirement IDs cited in the PR title/body in",
    "`docs/functional-requirements.md`. A change that contradicts a settled decision",
    "in docs/ is a finding even when it looks reasonable on its own — decisions are",
    "renegotiable with Evan, never unilaterally in a PR. These are the repo's",
    "non-negotiable invariants — a violation is a P0 and always blocks:",
    "",
    "- Logic in `cmd/farrier` or `internal/api` instead of `internal/core`; a",
    "  capability exposed to one frontend but not the other",
    "- Key material written to a log, an event, command output, or the bundle",
    "  directory (KEY-003)",
    "- A snapshot written unencrypted, or leaving the host before encryption",
    "- Verification weakened: a restore or promote path that proceeds past a failed",
    "  completeness, checksum, or cross-consistency check instead of refusing",
    "- A backup capture that can interleave with a push, or that interrupts reads",
    "- A restore that runs any version other than the one pinned in the snapshot,",
    "  or that runs schema migrations",
    "- Host identity not reinstalled on restore (SSH host keys, TLS), so existing",
    "  clones break",
    "- An operation that requires the machine that ran `init` (XCUT-001)",
    "- A new extension point that is not a published driver interface, or a driver",
    "  hardcoded instead of resolved through CORE-003",
    "- Scope creep beyond git/PR/review/secrets/CI/portability — issues, wikis, or",
    "  project management in any form",
    "- A new dependency or invented tooling (always flag)",
    "",
    "Also confirm `docs/status.json` was flipped for the cited IDs (`landed`, or",
    "`partial` with a `remaining` note if a slice remains).",
    "",
    "If the change touches the verification path, the snapshot format, key",
    "handling, or the identity model, say so prominently — those are the surfaces",
    "where a plausible-looking change silently breaks disaster recovery.",
  ].join("\n");
}

export function assignmentName(
  kind: "implement" | "pr-fix" | "pr-polish" | "pr-review",
  detail: string,
): string {
  switch (kind) {
    case "implement":
      return `Implement ${detail}`;
    case "pr-fix":
      return `Fix PR #${detail}`;
    case "pr-polish":
      return `Polish PR #${detail}`;
    case "pr-review":
      return `Review PR #${detail}`;
  }
}
