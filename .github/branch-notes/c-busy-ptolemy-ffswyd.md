# Branch: c/busy-ptolemy-ffswyd

Tracking branch for CORE-003 — the exec-based driver protocol: a Go
interface plus JSON-on-stdin/stdout exec protocol, so DNS, keystore, and
blob drivers (and any future extension point) can be satisfied by a
standalone executable with no Go required.

See `docs/functional-requirements.md` § CORE and `docs/status.json`.
