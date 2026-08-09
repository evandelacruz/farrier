# Branch: claude/rstr-004

Tracking branch for RSTR-004 — a restored instance must present the
original SSH host keys and TLS identity, so existing clones, remotes, and
known-hosts entries work unchanged.

Builds on RSTR-001/RSTR-002 (`internal/core/restore`): `restore.Restore`
already installs a snapshot's captured key material into the target
keystore (`installKeys`), mirroring `initialize.Run`'s write path, and then
runs `deploy.Up` against the placed state. What was missing: nothing ever
installed the SSH host key onto the *running* Forgejo service — not even on
an ordinary `up` on the instance's original host. `deploy.Up`'s existing
`configureTLS` step already resolves the persisted TLS certificate from the
keystore and ships it to Caddy on every run; this adds the same treatment
for the SSH host key, in a new `configureSSHHostKey` step: it resolves
`state.KeySSHHostKey`/`state.KeySSHHostKeyPublic` and writes them into
`GiteaStatePath` at the location `forge.RenderAppINI` now configures
explicitly via `SSH_SERVER_HOST_KEYS` (`forge.SSHHostKeyPath`).

Because `deploy.Up` runs for both `up` and `restore`, this closes the loop
end to end: the bundle's own key — generated once at `init` — is what
Forgejo actually serves from the very first deploy, so it's also what every
backup captures (`state.KeystoreKeyExporter` reads the keystore, not the
live host) and what `restore` reinstalls onto a fresh host, reproducing the
original instance's identity exactly.

See `docs/functional-requirements.md` § RSTR-004, `docs/tech-spec.md` §
"Deployment (`up`, ...)" and § "Restoring an instance (`restore`, ...)",
and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
