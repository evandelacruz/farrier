# Branch: claude/init-004

Implements **INIT-004**: `init` must refuse to overwrite an existing
`.farrier/` bundle, naming the folder, so re-running it against an
initialized project cannot clobber a live instance's identity.

Today `initialize.Run` validates the domain and the project folder, proves
zone control, generates key material, and then calls `Bundle.Save(dir)` —
which overwrites `farrier.yaml` and replaces `compose/` wholesale. Nothing
between those steps asks whether the folder already holds a bundle. A
second `farrier init` in an initialized project therefore spends an ACME
exchange, mints a fresh identity, and writes a manifest over the one the
live instance is running on.

Work:

1. **Teach `bundle` to recognize an occupied directory.** A small
   `bundle.Exists(dir)` that reports whether the directory already holds
   the files `Save` would overwrite — the manifest or `compose/`. The
   package that owns the layout owns the question.
2. **Refuse in `initialize.Run`'s validate step**, before zone-control
   proof and before any key material is generated, naming the resolved
   bundle folder — the default `.farrier/` or the explicit `-dir`.
3. **Prove it.** Tests that a second `Run` fails naming the folder, leaves
   the existing manifest byte-for-byte intact, spends no ACME proof, and
   stores no key material; that an empty or absent folder still
   initializes; and that the refusal covers a torn bundle (`compose/`
   with no manifest).

Core-only. `cmd/farrier` and the API surface are unchanged — the refusal
travels as an ordinary failed step on the CORE-002 event stream and a
nonzero exit. Nothing about snapshot verification, the four-kind state
model, or the identity model changes; this strengthens the identity
guarantee spec.md states under "Key material" — nothing may silently
overwrite a piece of it.

See `docs/functional-requirements.md` § INIT and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR
merges.
