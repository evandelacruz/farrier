# Branch: c/exciting-tesla-0xhcfp

Tracking branch for `docs/using.md` — "Working on your forge", the day-two
guide for an operator whose instance is already up.

The three user-facing documents cover standing an instance up (README),
protecting and recovering it (`docs/operating.md`), and what the operator is
accepting (`docs/security.md`). Nothing covered *using* it, which is why a
real question — where CI workflow files go — had no home in the repository.

Scope discipline: it covers what is different because this is Farrier, and
links to Forgejo's documentation for anything that is just Forgejo. Stated in
the doc's own opening so the boundary survives the next edit.

Docs only. No requirement changes state; `docs/status.json` is untouched.
