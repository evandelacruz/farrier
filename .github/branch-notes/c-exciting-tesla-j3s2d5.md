# Branch: c/exciting-tesla-j3s2d5

Tracking branch for wiring the nameless tier into `publish` (IMPT-004,
UP-006). A nameless bundle carries no domain, and `publish` built both the
`origin` URL and the pinned `known_hosts` line from the manifest's domain,
so publishing to a nameless instance failed outright. `publish` now takes
the instance's address, defaulting it from `-target`'s host.

See `docs/functional-requirements.md` § IMPT. No requirement changes state;
`docs/status.json` is untouched.
