# Branch: c/hopeful-ptolemy-cm3knk

Implements **UP-004**: forge state must live on a host directory
bind-mounted into the container that serves it, so recreating or replacing
a container never destroys it.

`orchestrate.Render` emits zero volumes for any service, so git
repositories and the SQLite database currently live in the forgejo
container's writable layer — an ordinary `up` can destroy them the moment
`Converge`'s `docker compose up -d --remove-orphans` recreates that
service. `deploy.Up` gains a `configureState` step that creates
`<RemoteDir>/state/git` and `<RemoteDir>/state/gitea` on the host, owned
by the uid:gid the official Forgejo image's `git` user runs as, and
bind-mounts them into the forgejo service at `forge.RepoRoot` and
`forge.DataPath` (tech-spec.md "Host state layout"). `forge.DataPath` also
covers LFS objects, attachments, avatars, and CI artifacts, so this one
mount keeps everything Forgejo itself writes durable.

See `docs/functional-requirements.md` § UP and `docs/status.json`.

This file exists for conductor tracking and can be deleted once the PR merges.
