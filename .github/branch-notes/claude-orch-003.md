# Branch: claude/orch-003

Tracking branch for **ORCH-003** — a target of `ssh://user@localhost` runs
the identical path as any remote host: no local mode, no branch on
locality, nothing skipped.

The invariant is structural (spec.md "A host is a host" — locality is an
argument, not a branch), so landing it means proving and locking it:
`orchestrate.Target` derives everything uniformly for loopback and remote
spellings, `*Client` issues the same commands whichever it believes it is
talking to, and a repo-wide guard fails the build if any product code ever
starts branching on whether a host is local.

See `docs/functional-requirements.md` § ORCH and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
