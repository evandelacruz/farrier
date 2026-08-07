# Branch: c/brave-rubin-26n3jr

Tracking branch for UP-003 — re-running `up` against a live host is safe
and converges that host to the bundle definition: admin bootstrap treats
an account that already exists as done rather than a failure, and the
shipped app.ini carries a content checksum so a config-only change is
visible to Converge's diff and forces the forgejo service to recreate.

See `docs/functional-requirements.md` § UP and `docs/status.json`.
