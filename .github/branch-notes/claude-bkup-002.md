# Branch: claude/bkup-002

Implements **BKUP-002**: `backup` must run against a live instance — reads
and fetches stay available throughout, and pushes may be held only for the
seconds of git capture.

Reopens PR #39, which paused to raise an architecture question: what
actually enacts the push-hold. Evan's answer (comment on #39) also found
that the capture order recorded in `docs/tech-spec.md` was wrong — tarring
bare repos scales with git data, so holding pushes across that tar would
make the hold grow with the instance. The corrected order, implemented
here:

1. Hold pushes
2. SQLite online backup
3. Record every repository's ref state (`backup.GitCapturer.Refs` — HEAD,
   packed-refs, refs/ — a few KB, instant)
4. Release pushes
5. Tar the (immutable, append-only) object store afterward, outside the
   hold (`backup.GitCapturer.Archive`, unchanged)

A push landing during step 5 can only add objects; it can never disturb a
ref pinned in step 3. The hold is now database-only and stays a second or
two regardless of git data size.

Mechanism: `backup.PushHold` (`Engage`/`Release`), with `CaddyPushHold`
reloading the bundle's already-running Caddy with a temporary Caddyfile
that returns 503 for git's smart-HTTP push endpoints
(`*/git-receive-pack`, `*/info/refs?service=git-receive-pack`) — rejected,
not queued — and releases by reloading back to the original, untouched
Caddyfile. `NoopPushHold` covers topologies with no proxy in front (local
capture, drills). The hold runs inside a configurable, low-default ceiling
and releases on every exit path — success, error, panic, canceled context.

Docs updated: `docs/tech-spec.md` (capture order, Backup impact target),
`docs/spec.md` (Backups section — operator-initiated, reject not queue,
database-only hold), `docs/functional-requirements.md` (BKUP-002 restated
testably), `docs/status.json`.

Core-only: no CLI/API wiring yet (API-001 still tracks that separately).

This file exists for conductor tracking and can be deleted once the PR
merges.
