# Branch: c/upbeat-brown-ea57ov

Implements **INIT-001**: `init` creates a bundle from a DNS name and a keystore target.

Adds the first CLI (`cmd/farrier init`) plus its core logic:
- `internal/core/initialize` — validates the domain and keystore target, resolves
  every component's image reference to a digest, renders Compose, and writes the
  bundle, emitting CORE-002 job events throughout.
- `internal/core/registry` — resolves a container image reference (tag or digest)
  to its pinned digest against any standard OCI/Docker-distribution registry.

See the PR description for what's deferred to INIT-002 (ACME zone-control proof)
and INIT-003 (key-material generation).

This file exists for conductor tracking and can be deleted once the PR merges.
