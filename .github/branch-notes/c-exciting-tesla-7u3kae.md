# Branch: c/exciting-tesla-7u3kae

Tracking branch for ACME-001 / ACME-002 — let the operator choose the ACME
server so the named tier can be rehearsed against Let's Encrypt staging
instead of production.

`init` gains `-acme-directory`, taking an ACME directory URL (`staging` is
shorthand for Let's Encrypt's staging directory). The choice is resolved to
an absolute URL and recorded in the manifest's ACME section, so every path
that issues or renews reads the bundle's own CA rather than defaulting back
to production: `init`, `attach`, and every host-converging path through
`deploy.configureTLS` — `up`, `promote`, `restore`, `drill`, `upgrade`.

`attach` gains the same flag, because a nameless bundle carries no ACME
section at all and naming one is where it first states its CA.

See `docs/functional-requirements.md` § ACME and `docs/status.json`.
