# Branch: c/exciting-tesla-e3wz5g

Carries the instance's SSH host **public** key in the bundle manifest, so
`publish` no longer needs read access to the keystore.

`publish` pins the instance's host key into a `known_hosts` entry so a host
answering with a different key fails the push instead of being silently
accepted. Today it gets that key by opening the bundle's keystore — the same
store holding `SECRET_KEY`, `INTERNAL_TOKEN`, and the age backup key — to read
a value that is public by definition. On a shared instance, that means nobody
but the instance's owner can publish, and letting them publish means handing
over every secret the instance has.

`init` now writes the public half into `farrier.yaml`, and `publish` reads it
from there. The private half stays in the keystore, untouched. A bundle whose
manifest predates the field falls back to the keystore, so existing bundles
keep working and the pin is never silently skipped.

Amends CORE-001 (a clause: public key material may live in the manifest,
nothing secret ever does), with the matching sentence in `docs/spec.md` and the
field in `docs/tech-spec.md`'s manifest field list. No new requirement ID, so
`docs/status.json` is untouched.

This file exists for conductor tracking and can be deleted once the PR merges.
