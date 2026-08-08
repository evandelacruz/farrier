# Branch: c/hopeful-ptolemy-yrl0y7

Tracking branch for **API-001** and **API-002** — the loopback HTTP RPC
server: `internal/api` binds `127.0.0.1:7433` by default, dispatches
`POST /init` and `POST /up` (the core operations landed so far) onto a
CORE-002 job each, and streams that job's progress over SSE from
`GET /jobs/{id}/events`.

API-001 is recorded `partial` in `docs/status.json`: the remaining RPC
verbs (`backup`, `restore`, `promote`, `upgrade`, `drill`, `import`,
`status`) get wired in as their core operations land — the API package,
loopback binding, job dispatch, and SSE framing are complete now.

See `docs/functional-requirements.md` § API and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
