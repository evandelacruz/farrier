# Branch: c/exciting-tesla-1aklgf

Tracking branch for UP-008, XCUT-003, and XCUT-004 — the three gaps that
live end-to-end testing turned up on 2026-08-10 and that nothing in the
backlog could represent, because every requirement read `landed`.

- **UP-008** — `up` must refuse to deploy a bundle onto host state that
  belongs to a different bundle. Today every deployment shares one state
  layout, so a second bundle boots Forgejo against the first's database
  with a mismatched `SECRET_KEY`. Silent data loss.
- **XCUT-003** — an operator-facing failure must name what failed, why,
  and what to do. Named cases: exhausted SSH auth, and an unwritable
  `-remote-dir` default.
- **XCUT-004** — CI must run `init`, `up`, and `publish` against a real
  Docker daemon over a real SSH connection and fail the build when that
  path breaks.

Docs-only: the three requirements enter `docs/functional-requirements.md`
and `docs/status.json` as `open`. `docs/spec.md` also gains the two
deployment shapes (one forge per project, one forge for many
repositories) with the operational trade stated — a clarification of
behavior that already exists, so no ID and no status entry. The README's
Status section stops claiming every requirement is implemented.

See `docs/functional-requirements.md` § UP and § XCUT, and
`docs/status.json`.
