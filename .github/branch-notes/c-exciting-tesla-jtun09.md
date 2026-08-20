# Branch: c/exciting-tesla-jtun09

Performance work in `internal/core/forge`, `internal/core/deploy`, and
`internal/core/bundle` (FORGE-005): every CI job on a colocated runner
re-downloads its toolchain, because `actions/setup-node` and friends read the
runner tool cache, job containers are fresh every run, and Farrier's carry no
tool cache at all. `up` now gives job containers a host directory mounted at
`/opt/hostedtoolcache` so the first job populates it and later jobs find it.

Second change on the same surface: the job-container image stops being a
constant and becomes a manifest field, defaulting to what the constant held.
An operator who wants closer GitHub parity can point it at a fatter image and
pay the pull; the quick start stays lean.

No requirement amendment — this is performance, not behavior — and no
`docs/status.json` change.

This file exists for conductor tracking and can be deleted once the PR merges.
