# Branch: c/exciting-tesla-ans5b9

Tracking branch for a defect fix in `internal/core/deploy` (UP-006,
FORGE-005): `up -address 127.0.0.1` on a colocated-runner deployment reports
every step green and produces an instance whose CI can never run. The
loopback address becomes `ROOT_URL` and the runner's registration address,
and inside a container both point at the container itself.

`up` now refuses a loopback address when it is deploying a colocated runner,
before it touches the host, naming an address the host actually answers on.

This file exists for conductor tracking and can be deleted once the PR merges.
