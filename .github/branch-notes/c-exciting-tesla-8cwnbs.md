# Branch: c/exciting-tesla-8cwnbs

Defect fix, no requirement ID: `farrier init` was not atomic. A real first
run stored all seven pieces of key material and then failed resolving
image digests, leaving no bundle on disk and the keystore holding a live
instance's worth of non-rotating key material. INIT-004's refusal could
not fire (no `.farrier/`), and a second `init` died inside the keystore's
rotation guard with an error that named no recovery path.

This branch makes `init` restartable: everything fallible that persists
nothing runs before the first `Store`, and a run that gets past that point
leaves a resume record in the bundle directory so the next `init` reuses
the key material it already wrote instead of colliding with it.

Touches `internal/core/initialize` only. `keystore`'s rotation guard is
unchanged — see `docs/spec.md` § "Key material".
