# Branch: claude/init-005

Tracking branch for INIT-005 — `init` accepting a project folder with no
domain, producing a nameless bundle that skips DNS-01 zone proof and
certificate issuance entirely and requires the operator to own nothing.
Every other piece of key material is generated exactly as usual, so a
nameless instance is a complete instance in all respects but its name.

See `docs/spec.md` § "Instances without a name",
`docs/functional-requirements.md` § INIT, and `docs/status.json`.
