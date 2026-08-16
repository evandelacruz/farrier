# Branch: c/exciting-tesla-vkk7xe

`up` mints the access token `farrier publish` needs, so the quick start never
leaves the terminal (FORGE-002, IMPT-004).

README §5 told the operator to "create an access token in the forge's web UI"
before `farrier publish` — the only step in the quick start that leaves the
terminal, with no word on where it lives or which scopes it needs. `up` already
execs into the forgejo container as the git user to create the first admin;
it now mints a scoped token for that account in the same step and hands it to
the operator in the credentials event that step already emits.

Repeating `up` mints nothing: the already-exists path says plainly that the
credentials were issued on the first deployment and where to create a new
token if they were lost, so tokens do not accumulate on the account.

The token is treated exactly like the admin password — a `keystore.Secret`,
redacted from every failure path, emitted once and nowhere else.

FORGE-002's requirement text is amended in this PR to name the token. That is
flagged at the top of the PR body.

This file exists for conductor tracking and can be deleted once the PR merges.
