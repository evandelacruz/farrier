# Branch: c/exciting-tesla-i8whv0

Implements **DRIL-002**: a drill instance must be quarantined — outbound
webhooks and email disabled, DNS untouched, reachable only through an SSH
tunnel.

`internal/core/drill` already restores the most recent snapshot onto a
scratch target and boots the full stack there. That instance carries
production's identity: its database holds production's webhook targets,
mailer configuration, and push mirrors, and its keystore holds production's
secrets. Until now nothing stopped it from acting on any of that. DRIL-001's
smoke CI job makes that immediate rather than theoretical.

Quarantine is a property of how a drill boots an instance, not a new
command, and not a special case around the smoke job.

Work:

1. **Render-time config override.** `forge.RenderAppINI` grows an options
   struct with a `Quarantine` flag. When set, the rendered `app.ini`
   disables webhooks, the mailer, and mirrors outright. A normal `up`
   renders exactly what it rendered before.
2. **No publicly published port.** `deploy.Up` publishes Caddy's HTTPS port
   bound to the host's loopback on the drill path instead of on every
   interface, so the instance is reachable through an SSH tunnel and from
   nowhere else.
3. **Carry the flag, don't infer it.** `deploy.Options.Quarantine` and
   `restore.Options.Quarantine` pass through; `drill` is the only caller
   that sets it, and it sets it unconditionally.
4. **Prove DNS stays untouched.** Already true — `drill` never imports
   `internal/core/dns`. A test pins it so a later change cannot quietly
   break it.

Nothing about snapshot verification, the four-kind state model, or the
identity model changes. `up`, `promote`, and `restore` behave exactly as
before.

See `docs/functional-requirements.md` § DRIL and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR
merges.
