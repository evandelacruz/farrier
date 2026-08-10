# Branch: c/exciting-tesla-ks300r

Tracking branch for a fix to UP-001 and UP-003: `up`'s wait for Forgejo
waits for the thing that actually has to be true. It probed
`docker compose exec -T forgejo true`, which proves the container is
running and nothing about Forgejo — so on a host with a fresh state
directory, admin bootstrap opened a database whose schema Forgejo's first
boot was still migrating into existence and failed with
`CreateUser: no such table: user`.

See `docs/functional-requirements.md` § UP. No requirement changes state;
`docs/status.json` is untouched.
