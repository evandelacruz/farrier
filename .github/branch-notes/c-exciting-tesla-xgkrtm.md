# Branch: c/exciting-tesla-xgkrtm

Runs the admin-bootstrap command as the `git` user, and stops the three
`docker compose exec` failure paths from throwing away half the output they
were meant to report.

`docker compose exec` defaults to root, and Forgejo refuses to run as root:
`up` aborted with "Forgejo is not supposed to be run as root. Sorry." on every
host. The two sibling execs into the same container — runner registration and
the drill smoke job — already pass `-u git`; admin bootstrap did not. It does
now.

The abort was reported to the operator as "create admin account: command
failed with no output", because Forgejo's CLI wrote that fatal to stdout and
all three call sites discarded stdout and captured only stderr. They now
capture both and report whichever carries the message. The reason those paths
avoid the transport's own error text — it embeds the whole command, including
the quoted admin password — is unchanged; that argues against using the error,
not against reading the command's output. Bootstrap additionally redacts the
password from anything it reports, so a command that echoes it back cannot
turn a failure message into a leak.

A fix with no requirement ID, so `docs/status.json` is untouched.

This file exists for conductor tracking and can be deleted once the PR merges.
