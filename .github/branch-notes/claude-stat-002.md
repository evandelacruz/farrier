# Branch: claude/stat-002-2

Tracking branch for STAT-002 — replication lag reporting.

Per Evan's decision on PR #37 (closed, no code): build now against the
published `blob.Adapter` interface rather than waiting on `backup --to`
(BKUP-002..005). This PR:

1. Adds `Modified time.Time` to `blob.Object` — a change to a published
   interface — populated by `local`, `s3`, and the CORE-003 exec protocol
   (documented as a new `modified` field; missing from an older third-party
   adapter reads as zero time, i.e. unknown).
2. Adds `internal/core/status.ReplicationLag`, computed from a
   `state.BlobExporter`'s newest object `Modified` time. No destination, no
   objects, or no known `Modified` time all report truthfully as unmeasured
   rather than a fabricated number. Operator-assembled transports stay
   unmeasured per spec.md "Replication lag" — nothing here invents
   bundle-level destination config.

Core-only: CLI/API wiring lands with `backup --to` (BKUP-005).

See `docs/functional-requirements.md` § STAT and `docs/status.json`.
