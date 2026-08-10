# Branch: c/exciting-tesla-kn755a

Delivers **KEY-004**: the `store` method on the exec keystore protocol.

Today an out-of-tree keystore driver — any driver name that is not `file`
or `command`, reached as a standalone executable over the CORE-003 exec
protocol — can only serve key material back. It cannot receive it, so an
operator whose secrets live behind their own driver executable cannot let
`init` mint into it and is pushed back to the `file` driver with a
plaintext copy on disk.

Work:

1. **`store` on the wire.** Method `store`, params
   `{"key": keyName, "secret": "<base64>"}`, empty result — the shape
   `docs/tech-spec.md` "Keystore driver config" already settles. Base64
   for the same reason `resolve` uses it: JSON is text-only and key
   material is arbitrary bytes.
2. **Store capability decided from config, at construction.** `config.store: true`
   declares the executable implements the method; absent or `false` means
   resolve-only, and the constructor returns a type that does not
   implement `keystore.Writer`. This is the constraint KEY-004 exists for:
   `initialize.Run` type-asserts `Writer` at `StepValidate`, so the answer
   has to come from config — one `execDriver` Go type serves every
   executable, so a `Store` method implemented unconditionally would make
   every exec keystore pass the assertion and surface resolve-only-ness
   only after ACME DNS-01 proving and key generation.
3. **KEY-003 on the error path.** A failing driver executable's stderr
   reaches the error Farrier surfaces; the secret is scrubbed from it in
   both its raw and base64 forms, as the `command` driver already does.
4. **`resolve` states absence.** Evan's call on review: the result carries
   `{"secret": "<base64>", "found": true|false}`, and `found: false` is
   the protocol's only positive "not found". The rotation guard is
   fail-closed on this lookup, so a driver has to *positively determine* a
   key is absent — `found: true` with no secret, or a response omitting
   `found`, is malformed and refuses the write rather than reading as an
   empty slot.

Declaring `store: true` against an executable that does not implement the
method still fails at the first call. That is the operator misconfiguring
their own driver, and `docs/tech-spec.md` already records it as the one
case config cannot catch — nothing probes the executable to close it.

The generic exec protocol in `internal/core/driver` needs no change:
`Invoke` already handles a call with no result.

Nothing about snapshot verification, the four-kind state model, or the
identity model changes.

See `docs/functional-requirements.md` § KEY and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR
merges.
