# Branch: c/exciting-tesla-vje78p

Implements **DRIL-003**: `drill` must leave the scratch target clean on
completion.

A drill restores production's most recent snapshot onto a scratch target and
boots the full stack there. Today it walks away and leaves that stack
running, with production's git repositories, database, app.ini, runner
secret, and SSH host key sitting on the scratch host indefinitely. The
rehearsal is disposable; what it leaves behind is not.

Clean has to mean clean on every exit path — a successful drill, a drill that
failed at `verify-snapshot`, a drill that failed at `wait-forge` with a
half-booted stack, a canceled context, a panic. The half-booted case is the
one this requirement exists for, and the one most likely to be missed,
because the happy path is the easy one to test.

Work:

1. **`deploy.Down`** — the mirror of `deploy.Up`, in the package that owns
   the host state layout: stop and remove the Compose project's containers,
   networks, and volumes, then remove `RemoteDir` itself.
2. **A `teardown` step in `drill`**, emitted on the same CORE-002 stream as
   every other drill step, running from a deferred call so it happens on
   success, on a failed step, and while a panic is unwinding.
3. **A teardown context that outlives cancellation**, so a canceled drill
   still cleans up instead of leaving the most state behind at exactly the
   moment the operator hit Ctrl-C.
4. **`Report.Teardown`**, so a teardown failure is reported rather than
   swallowed: the operator has to know a scratch target still holds
   production's state.

Nothing about snapshot verification, the four-kind state model, or the
identity model changes. DRIL-001's smoke CI job is not implemented here; it
is still `partial` and lands in its own pass.

See `docs/functional-requirements.md` § DRIL and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR
merges.
