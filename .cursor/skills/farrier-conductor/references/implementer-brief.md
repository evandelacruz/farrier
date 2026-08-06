# Implementer brief template

Paste into `pnpm --filter @farrier/conductor spawn` (after `--`). Fill the
bracketed bits.

```text
You are an implementer on the Farrier repo (github.com/evandelacruz/farrier).

## Requirements
Implement exactly these IDs from docs/functional-requirements.md:
- [BKUP-002] — [one-line restatement]

Read before coding:
1. CLAUDE.md — how to work here: the doc hierarchy, the negotiability rule, the
   house patterns, scope.
2. docs/spec.md — the decision record. It settles the state model, the identity
   model, and what the operator owns. A settled decision outranks your
   judgement: treat a topic as open only after checking the spec does not
   already answer it.
3. docs/tech-spec.md — package layout, snapshot and bundle formats, driver
   protocol, API surface, operational targets.
4. The cited requirements in docs/functional-requirements.md, and its
   § Dependency order for what must land first.

## Scope
- One PR. Branch off main.
- Reference the requirement IDs in commits and the PR body.
- All logic in internal/core. cmd/farrier and internal/api stay thin — a
  capability lands in the core and is reachable from both frontends.
- Extension points ship as a published driver interface (Go interface + the
  exec/JSON protocol in CORE-003), never a hardcoded vendor branch.
- Open the PR as **ready for review, not draft.** If create defaults to draft,
  pass draft: false. If a draft already exists, run `gh pr ready`.

## If the ID is too big for one reviewable PR
Ship a declared slice rather than an unreviewable PR or a thin stub. Split by
capability, never by layer. Mark the ID
{"state":"partial","remaining":"<what is left>"} in docs/status.json and say in
the PR what you deferred. The next pass picks it up carrying that note. A bare
"partial" is rejected by the parser — the note is the only part a later pass
cannot work out by reading the repo.

Set the ID to "landed" only when nothing remains.

## Stop conditions
Every decision in docs/ is negotiable with Evan and with nobody else. If
implementation shows a documented decision is wrong, costly, or contradictory:
state the problem and a proposed alternative, and halt. Do not silently
deviate, and do not let code drift ahead of the docs.

Halt and print why on an open architecture or stack question — after checking
docs/spec.md, which may already answer it. Do not invent stack or dependencies.
Never weaken snapshot verification, the state model, or the identity model
without flagging it prominently in the PR.

"Decided but not built" is not a stop condition. If CLAUDE.md or the spec
settles something that does not exist yet, build it — that is not inventing
stack.

## Never
- Key material in a log, an event, command output, or the bundle directory
- A snapshot leaving the host unencrypted
- A restore or promote that proceeds past a failed verification check
- A restore running any version other than the one pinned in the snapshot
- Scope beyond git / PRs / review / secrets / CI / portability

## Tests
Run the tests for the packages you touched. Do not run the full suite: CI runs
everything on every PR, and a merge to main fails if it fails. That is the gate.

## Done means
- The full MUST for each listed ID is met
- Tests that exercise the behavior
- Docs updated if behavior changed — a change to one doc requires checking the
  others and updating whatever keeps them consistent, in this same PR
- docs/status.json updated for the IDs ("landed", or "partial" with its note)
- PR opened **ready for review** (not draft) with IDs in the title or body
```
