# Branch: claude/stat-001

Tracking branch for STAT-001 — `status` reporting instance health: services
up, TLS validity, disk headroom, and last-backup age.

This slice lands services up, TLS validity, and disk headroom. Last-backup
age is deferred: it needs the snapshot listing/naming convention that
BKUP-001..005 (not yet landed) will define for its destination — see
`docs/tech-spec.md` § "Status (`status`, STAT-001)" and `docs/status.json`.

See `docs/functional-requirements.md` § STAT and `docs/status.json`.
