# Branch: claude/core-002

Tracking branch for CORE-002 — the job/progress event model: every
long-running operation is a job identified by ID, emitting a single ordered
event stream (`jobId`, `step`, `state`, `detail`, `timestamp`) that both
frontends render.

See `docs/functional-requirements.md` § CORE and `docs/status.json`.
