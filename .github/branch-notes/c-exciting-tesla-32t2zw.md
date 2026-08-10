# Branch: c/exciting-tesla-32t2zw

Finishes **UP-002**: *given a named bundle*, `up` must complete with the
forge serving HTTPS at the bundle domain and usable in a browser
immediately.

The HTTPS completion path already ships. What was left is the conditional
INIT-005 introduced: a nameless bundle has no domain, so it has no HTTPS
endpoint for `up` to complete against. This branch makes `up`'s guarantee
explicitly a guarantee about named bundles, and makes a nameless bundle
fail up front — before anything on the host is touched — naming UP-006 as
the requirement that will teach `up` to serve one over plain HTTP.

UP-006 is deliberately *not* implemented here. The gap is left a gap so
`up` cannot silently half-work against a nameless bundle.

See `docs/functional-requirements.md` § UP and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
