# Branch: c/zen-lovelace-yab34g

Implements **INIT-002**: `init` proves control of the DNS zone via an ACME
DNS-01 challenge and fails, with the reason, when proof fails.

Adds a zone-control-proof step to `internal/core/initialize.Run`, backed by
`internal/core/acme.Issue` (ACME-001) against an operator-named lego DNS-01
provider, independent of the bundle's own DNS driver.

See `docs/functional-requirements.md` § INIT and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
