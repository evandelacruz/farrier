# Branch: c/exciting-tesla-u3occw

Tracking branch for FORGE-005 — `up` deploys a colocated Forgejo Actions
runner, registered against the instance without operator action, so a
workflow pushed to a fresh deployment runs. The runner reaches the host's
Docker socket to start job containers (spec.md "CI trust boundary" > "The
colocated runner holds the host's Docker socket"), and an operator who does
not want that on the forge host disables the colocated runner in the
manifest and registers a remote one instead.

See `docs/functional-requirements.md` § FORGE and `docs/status.json`.
