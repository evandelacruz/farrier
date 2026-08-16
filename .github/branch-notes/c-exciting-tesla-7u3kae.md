# Branch: c/exciting-tesla-7u3kae

Tracking branch for ACME-001 / ACME-002 — let the operator choose the ACME
server so the named tier can be rehearsed against Let's Encrypt staging
instead of production.

`init` gains `-acme-directory`, taking an ACME directory URL (`staging` is
shorthand for Let's Encrypt's staging directory). The chosen value is
resolved at `init` and recorded in the manifest's ACME section, so every
path that issues or renews — `init`, `up`, `attach`, `promote`, `restore` —
reads the bundle's own CA rather than defaulting back to production.

See `docs/functional-requirements.md` § ACME and `docs/status.json`.
