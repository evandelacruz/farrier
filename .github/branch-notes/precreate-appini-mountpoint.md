# Branch: claude/precreate-appini-mountpoint

Creates the file the `app.ini` bind mount lands on before `converge` starts the
containers, so `up` stops failing on macOS.

Two of the forgejo service's bind mounts overlap by design:
`<RemoteDir>/state/gitea` mounts at `/data/gitea`, and `<RemoteDir>/forge/app.ini`
mounts at `/data/gitea/conf/app.ini` — inside the first. The container runtime
therefore has to create the second mount's target file host-side, under
`state/gitea/conf/`. On Docker Desktop that path is reached through virtiofs and
the runtime refuses, so `up` pulled every image, created all three containers,
and then died starting `farrier-forgejo` with `mountpoint ... is outside of
rootfs`. On Linux the same nesting works.

`configure-state` now creates `state/gitea/conf/app.ini` itself, next to the
state directories it already creates, so the runtime binds over a file that is
already there instead of having to make one. Same command on every host — no
OS detection and no check for whether the target is local, which is the
locality-dependent behavior the deployment path exists to avoid. The file is
created only when absent and never truncated, so a re-run against a live host
leaves a real `app.ini` sitting there byte-for-byte alone. The path is derived
from `deploy.GiteaStatePath` and the container-side `forge.AppINIPath`, not
spelled out a third time.

Not taken: writing the rendered `app.ini` straight into `state/gitea/conf/` and
dropping the second mount. That would work too, and it moves a file containing
`SECRET_KEY` inside the backed-up state and raises a restore-ordering question
about a freshly rendered config being clobbered by a restored one.

A fix, so no requirement ID and `docs/status.json` is untouched.
`docs/tech-spec.md`'s host state layout gains one line for the file.

This file exists for conductor tracking and can be deleted once the PR merges.
