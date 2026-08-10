# Branch: c/exciting-tesla-1gsxez

Tracking branch for **UP-005** — `up` serves git over SSH at the bundle
domain, published on a host port the manifest declares and defaulting to
2222, using the bundle's SSH host key, so `git clone` and `git push` over
SSH work against a fresh deployment and the SSH clone URL Forgejo displays
is the one that works.

See `docs/functional-requirements.md` § UP and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
