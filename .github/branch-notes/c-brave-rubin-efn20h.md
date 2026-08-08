# Branch: c/brave-rubin-efn20h

Implements **UP-002**: `up` must complete with the forge serving HTTPS at
the bundle domain and usable in a browser immediately.

Adds TLS/Caddy provisioning to `internal/core/deploy.Up`: issues a
certificate for the bundle domain via ACME DNS-01 (`internal/core/acme.Issue`,
ACME-001), renders Caddy's config in a new `internal/core/caddy` package,
ships both to the host, mounts them into the caddy service, publishes its
HTTPS port, and waits for caddy to accept commands before `up` returns.

The ACME DNS-01 provider name and contact email `init` already collects
(INIT-002) are now persisted into the manifest (`bundle.ACMEConfig`) so `up`
can reissue certificates through the same provider without asking again.

See `docs/functional-requirements.md` § UP and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
