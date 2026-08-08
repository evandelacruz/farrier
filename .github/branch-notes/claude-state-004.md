# Branch: claude/state-004

Tracking branch for STATE-004 — key material enumerable as a state kind for
capture and installation: `internal/core/state.KeyExporter` enumerates the
fixed set of key names a bundle's key material consists of (the three
Forgejo secrets `forge.ResolveSecrets` already reads, plus TLS certificate
material and SSH host keys, spec.md "Identity" > "Key material") and
resolves each through a `keystore.Driver` — ready for `backup` (BKUP-001) to
capture into a snapshot's `keys/` directory and for `restore` (RSTR-001) to
install back onto a target.

See `docs/functional-requirements.md` § STATE and `docs/status.json`.
