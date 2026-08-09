# Branch: claude/fail-005

Implements **FAIL-005**: remote runners must reconnect after promotion with
no re-registration.

spec.md "Identity lives in the bundle, not the host" > "Runners across
relocation" already settles this as a property of promotion, not a
mechanism to build: "Runner registrations live in the database, and
runners dial out to the domain. After promotion, remote runners reconnect
automatically."

Both halves already hold by construction:

- `restore.placeState` streams the snapshot's whole database file onto the
  standby host (`cat > .../gitea.db`) — every table, including Forgejo
  Actions' `action_runner` registrations, arrives verbatim. `forge.ReconcileCI`
  (FORGE-004/FAIL-003), the only code that touches the restored database
  before it lands, updates only `action_run`/`action_run_job` status
  columns and never runs a query against `action_runner`.
- `forge.RenderAppINI` derives `DOMAIN`, `ROOT_URL`, and `SSH_DOMAIN` solely
  from the bundle manifest's domain — never from `deploy.Host` or any
  host-specific address. `promote.Options.DNSValue` (the standby host's own
  address) flows only into the DNS flip (FAIL-004); it never reaches
  rendered config. A remote runner already pointed at the bundle domain
  needs no reconfiguration once DNS-004's 60-second TTL catches up.

This PR adds the tests that prove both halves rather than building a new
reconnection mechanism:

- `internal/core/forge`: `TestReconcileCILeavesRunnerRegistrationUntouched`
  — seeds an `action_runner` row alongside orphaned running jobs, runs
  `ReconcileCI`, and asserts the registration row is byte-for-byte
  unchanged while the orphaned jobs still reset to queued.
- `internal/core/promote`: `TestPromoteRemoteRunnerReconnectsWithoutReregistration`
  — runs a full `Promote` against a snapshot whose database carries a
  registered remote runner, and asserts (a) the registration lands on the
  standby host unchanged and (b) the app.ini shipped to the standby host
  carries the bundle's domain and never the standby host's own address
  (`opts.DNSValue`).

See `docs/functional-requirements.md` § FAIL and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
