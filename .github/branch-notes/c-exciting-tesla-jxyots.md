# Branch: c/exciting-tesla-jxyots

Tracking branch for IMPT-004 — `farrier publish` on the documented happy
path. When the instance account has no SSH key registered and the operator
named no `-ssh-key`, publish falls back to the operator's own public key
(`~/.ssh/id_ed25519.pub`, then `~/.ssh/id_rsa.pub`) and says in the
authorize event which file it registered. README § 5 gains the two
prerequisites a live run hit: where the access token is made and which
scopes it needs, and that publish registers a key on the account.

See `docs/functional-requirements.md` § IMPT and `docs/status.json`.
