# Branch: claude/acme-002

Tracking branch for ACME-002 — automatic certificate renewal actually
persists. Adds a keystore-wide rotation registry (`keystore.Rotates`) so
`keystore.New` enforces "non-rotating key material can never be
overwritten" once, above every driver, instead of `FileDriver.Store`
guarding only itself. The TLS certificate and its private key are the one
declared exception; `deploy.configureTLS` writes a renewed certificate back
through the keystore on the rare renewal-due branch, so the next `up`
decides from the fresh certificate instead of re-renewing forever.

See `docs/functional-requirements.md` § ACME and `docs/status.json`.
