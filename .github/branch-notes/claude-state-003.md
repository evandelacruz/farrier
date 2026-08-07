# Branch: claude/state-003

Tracking branch for STATE-003 — blobs exportable through the blob adapter
interface: `internal/core/state.BlobExporter` is the read side of
`blob.Adapter` (List, Get), so every shipped adapter (`local`, `s3`) and any
third-party exec adapter (CORE-003) already satisfies it with no adapting
code, ready for `backup` (BKUP-001) to capture into a snapshot's `blobs/`
directory.

See `docs/functional-requirements.md` § STATE and `docs/status.json`.
