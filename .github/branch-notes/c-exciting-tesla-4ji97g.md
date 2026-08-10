# Branch: c/exciting-tesla-4ji97g

Tracking branch for a fix to FORGE-002: the first admin account is created
under a username Forgejo will accept. Forgejo reserves `admin`, so
`forgejo admin user create` refused it and `up` could not provision its
first admin on any host — the operator finished a deployment with no way
into the forge.

See `docs/functional-requirements.md` § FORGE. No requirement changes
state; `docs/status.json` is untouched.
