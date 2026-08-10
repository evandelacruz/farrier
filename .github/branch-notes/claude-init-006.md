# Branch: claude/init-006

Tracking branch for INIT-006 — `init` reports where each piece of key
material was stored, naming the keystore driver and, when the driver says,
its target, and states that the age backup key is the one unrecoverable
loss. Reported through the CORE-002 event stream, so the CLI and the
dashboard show the same thing, and never carrying key material itself
(KEY-003).

See `docs/functional-requirements.md` § INIT and `docs/status.json`.
