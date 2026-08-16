# Branch: c/exciting-tesla-5k65py

Docs only — no requirement IDs change and no delivery state moves.

Rewrites the dogfooding milestone in `docs/spec.md` from "Farrier hosts
Farrier" to **"Farrier hosts a real project"**: the acceptance test runs
against a private project with real pull-request and CI traffic, not this
public repository. "Farrier hosts Farrier" returns as a distinct, later
milestone, gated on porting the agent fleet off the GitHub API.

Reconciles the milestone wording in `CLAUDE.md`, `AGENTS.md`, and
`.claude/skills/farrier-conductor/SKILL.md`.

This file exists for conductor tracking and can be deleted once the PR merges.
