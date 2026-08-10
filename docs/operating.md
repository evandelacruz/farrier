# Operating Guide

How to protect a Farrier instance and get it back. Recovery comes first, because that is the section people read under pressure.

Design rationale lives in [spec.md](spec.md); this file is the operator's runbook.

Every command below runs on your own machine. Farrier installs nothing on the forge host — it opens an SSH session, does the work, and exits — so all of this works when the host is unreachable, on fire, or gone.

## You have lost the server

Answer one question: **do you have a snapshot?**

### Yes — promote onto a new host

```bash
farrier promote \
  -bundle ./bundle \
  -target ssh://user@new-host \
  -from s3://my-bucket/farrier
```

This restores the latest snapshot onto the new host, verifies it, starts services, resets orphaned CI jobs so they re-dispatch, and flips DNS. It prints the snapshot's age and waits for your confirmation first — read that number, it is what you are about to accept as lost.

Everything comes back: repositories, full history, pull requests, review comments, CI history, secrets, LFS objects. The new instance presents the original TLS certificate and SSH host key, so existing clones, remotes, and `known_hosts` entries keep working untouched. Nobody re-points anything.

`-snapshot <key>` restores a specific snapshot instead of the newest — use it when the latest one captured something you are trying to escape.

### No — deploy the bundle and rebuild from your clones

```bash
farrier up -bundle ./bundle -target ssh://user@new-host
```

You get a working, correctly-identified forge with nothing in it. Same domain, same TLS, same SSH host key — clients cannot tell it apart from the old one. Then push your repositories back from any local clone.

What you do not get back: pull requests, review comments, and CI history. Those only ever lived in the database. Your CI secrets are gone too, but those are recoverable by hand — you re-issue or re-paste them from wherever they came from.

The part that survives regardless is your forge's identity. Key material lives in your keystore, not on the host and not in the database, so a dead server with no backup still cannot cost you it.

## Backups

### What a snapshot contains

All four kinds of state, plus a manifest with per-component checksums: git repositories, the SQLite database, blobs (LFS objects, CI artifacts, avatars), and the bundle's key material. One age-encrypted archive per backup, encrypted on the forge host before it moves.

### Running one

```bash
farrier backup \
  -bundle ./bundle \
  -target ssh://user@forge-host \
  -to s3://my-bucket/farrier
```

`-to` takes an S3-compatible URI or a filesystem directory. There is no default: name a destination every time.

**Do not point it at the forge host.** A filesystem path resolves on your machine, which is the point — a backup that dies with the server is not a backup.

The instance stays live throughout. Reads and fetches never stop. Pushes are held for the second or two the database capture takes, and a push arriving in that window is rejected outright with an explicit message rather than queued, so the client sees a clean failure and retries. The hold does not grow with the size of your git data.

The snapshot is verified at creation, against the decrypted form of the exact bytes about to be written. A backup that fails verification fails loudly and exits nonzero naming the defect — it does not write a bad snapshot and hope.

### How often

Your call. Farrier ships no scheduler and nothing runs a backup on your behalf, so if you never run it there is nothing to promote to. Your recovery point is exactly your backup cadence.

The minimum-effort route is a cron entry. It can live on your own machine, or on the forge host itself — the forge host keeps running when your laptop is closed, at a key-custody cost worth reading first ([security.md](security.md), "Backups are exactly as private as the age key"):

```cron
0 * * * * /usr/local/bin/farrier backup -bundle /path/to/bundle -target ssh://user@forge-host -to s3://my-bucket/farrier
```

### Checking where you stand

```bash
farrier status -bundle ./bundle -target ssh://user@forge-host -to s3://my-bucket/farrier
```

Reports services, TLS validity, disk headroom, and the age of the newest snapshot at that destination. It warns when the certificate is within 14 days of expiry.

## Restore

`restore` is `promote` without the DNS flip and CI reconciliation — use it to build an instance from a snapshot without cutting traffic over.

```bash
farrier restore \
  -bundle ./bundle \
  -target ssh://user@fresh-host \
  -from s3://my-bucket/farrier
```

It refuses to proceed on a snapshot that fails verification, naming the specific missing or torn state rather than restoring something partial. It also boots the exact Forgejo version recorded in the snapshot, not the newest one — schema migrations run during `upgrade` and at no other time, so a restore can never migrate your data out from under you.

## Drill: proving it works before you need it

```bash
farrier drill \
  -bundle ./bundle \
  -target ssh://user@scratch-host \
  -from s3://my-bucket/farrier
```

Restores your latest snapshot onto a scratch target, boots the full stack, runs a smoke CI job, reports success or the specific failing step, and tears the target down.

The drilled instance is quarantined, because it carries your production identity: outbound webhooks, email, and mirrors are disabled, DNS is untouched, and it publishes no routable port — you reach it through an SSH tunnel. The rehearsal happens with the outside world hearing nothing.

The scratch target can be any Docker host, including your own machine.

This is the difference between having backups and knowing they restore. Run it on a schedule that matches how much you would mind being wrong.

## Upgrades

```bash
farrier upgrade \
  -bundle ./bundle \
  -target ssh://user@forge-host \
  -to s3://my-bucket/farrier \
  -image codeberg.org/forgejo/forgejo:12
```

Runs only against a healthy instance, and in this order: back up, bump the pinned version, apply migrations, verify. `-to` is required because the pre-upgrade backup is the way back — a failed upgrade leaves you with that snapshot and a working path to the version you were on.

## What a backup does not carry

- **CI secrets are in the snapshot** (they live in the database, encrypted with your `SECRET_KEY`) — but they do not survive a server lost *without* one. Re-paste them from their issuers.
- **A nameless instance's address is its identity.** If you deployed without a domain, moving to a new host means a new address, and everyone re-points their remote once. A named instance relocates by DNS flip and nobody changes anything.
- **Your app is not in here.** Farrier hosts your repository. Where CI deploys your application is your workflow's business.

## The one unrecoverable loss

Snapshots are age-encrypted and you hold the only key. **Lose that key and every backup you own is permanently unreadable.** There is no recovery path, no reset, and nobody to ask — that is what makes the snapshots safe to store anywhere.

Keep it where you would keep the thing you cannot re-derive, and keep it somewhere other than the machine you would be recovering from.

The same applies to the bundle itself. It holds no key material, but it holds your instance's definition, so at least one copy must live somewhere the instance does not serve.
