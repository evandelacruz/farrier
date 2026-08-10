# Branch: c/exciting-tesla-7k5o7w

Finishes **KEY-002**: the `command` keystore driver's write side.

The resolve half shipped already — `config.command` runs through `sh -c`
with `FARRIER_KEY_NAME` in its environment and its stdout is the secret.
What is left is the half `docs/status.json` records as remaining: with no
write side, `init` cannot mint key material straight into an operator's
secret manager and forces a `file` keystore with a plaintext copy on disk.

Work:

1. **`config.storeCommand`** — a second command, same `sh -c` +
   `FARRIER_KEY_NAME` shape, receiving the secret on **stdin** and exiting
   zero on success. Never argv, never the environment, never a log line
   (KEY-003).
2. **Store capability decided at construction**, per KEY-004's timing
   constraint and `docs/tech-spec.md` "Keystore driver config": with no
   `storeCommand`, the constructor returns a type that does not implement
   `keystore.Writer`, so `initialize.Run`'s type assertion fails at
   `StepValidate` — before ACME DNS-01 proving and before any key material
   exists.
3. **A positive "not found"** from the resolve command, so the rotation
   guard can tell absence from failure. Without one, the guard's
   fail-closed lookup refuses every store of fresh non-rotating key
   material and `init` could never mint through this driver.

Nothing about snapshot verification, the four-kind state model, or the
identity model changes. KEY-004 (the exec protocol's `store` method) is a
separate ID and is not touched.

See `docs/functional-requirements.md` § KEY and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR
merges.
