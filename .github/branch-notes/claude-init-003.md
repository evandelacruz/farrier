# Branch: claude/init-003

Tracking branch for INIT-003 — `init` generating all bundle key material:
Forgejo `SECRET_KEY` and `INTERNAL_TOKEN`, the LFS JWT secret, TLS
certificates, SSH host keys, and the age backup key, persisted through the
bundle's configured keystore driver.

See `docs/functional-requirements.md` § INIT and `docs/status.json`.
