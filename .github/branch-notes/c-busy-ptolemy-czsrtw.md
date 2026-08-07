# Branch: c/busy-ptolemy-czsrtw

Tracking branch for ACME-001 and ACME-002 — in-process TLS certificate
issuance and renewal via ACME DNS-01, using lego: `internal/core/acme`
issues certificates, fails with the CA's or DNS provider's own reason on
challenge failure, and exposes the renewal-due and expiry-warning checks
`status` will surface once STAT-001 lands.

See `docs/functional-requirements.md` § ACME and `docs/status.json`.
