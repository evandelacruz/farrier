# Security Considerations

What you are accepting by running a Farrier instance, and what you can do about each item. This is the operator's list; the decisions behind it are recorded in [spec.md](spec.md), and the procedures are in [operating.md](operating.md).

A Farrier instance is single-tenant by design. It runs your team's own code, on a host you own, with you as the only administrator. Every item below is stated against that assumption — none of it describes a system hardened for running code you do not trust.

## CI can take the forge host

The colocated Actions runner holds the host's Docker socket, because Actions runs every job step in a container and a container cannot create containers on its own. Anything that reaches that socket can start any container, including one mounting the whole host filesystem.

**So a workflow that runs on a default deployment can take the forge host** — the same host holding your git data and database. "Isolated at the container level" reads stronger than what ships, which is why it is stated here plainly.

The realistic threat is not a teammate writing something hostile. It is a compromised build dependency executing during a job — a risk every CI system carries. What changes here is blast radius, because the runner sits on the machine holding the state.

**What to do about it:** disable the colocated runner and register a remote one against your domain. The remote runner's own host still holds a Docker socket — the risk moves rather than disappearing — but it moves onto a disposable machine carrying no forge state. Nothing else about the instance changes; runner registrations live in the database and survive backup, restore, and promotion either way.

This is topology, not a configuration knob. See spec.md, "The colocated runner holds the host's Docker socket."

## Backups are exactly as private as the age key

Snapshots are age-encrypted before they leave the host, and you hold the only key. That is what makes them safe to store in a bucket you do not fully control.

Two consequences, and the second one is easy to walk into:

**Lose the key and every snapshot is permanently unreadable.** No recovery path, no reset, nobody to ask. Keep it somewhere other than the machine you would be recovering from.

**Running backups from the forge host puts the key on the forge host.** Host-side cron is a legitimate pattern — `farrier backup -target ssh://user@localhost -to s3://...` works, and it means backups keep running whether or not your laptop is open. But the backup process has to resolve the age key to encrypt, so it must be reachable from that machine.

It is the *combination* that opens the hole, not either half:

| Keystore | Cron runs on | Exposure |
|---|---|---|
| `file` on disk | your machine | Host never sees the key |
| `command` (1Password, Vault, `pass`) | forge host | Key fetched per run, never at rest there |
| **`file` on disk** | **forge host** | **Root on the host decrypts every snapshot in the bucket** |

The last row is the one to avoid. Note what it does and does not cost you: someone with root on that host already has your source, your database, and your CI secrets by standing there. What the key additionally gives them is your *history* — every earlier snapshot — and the credentials to delete the backups you would recover with.

**What to do about it:** use the `command` keystore driver if the cron lives on the forge host, so the key is fetched at backup time rather than sitting on disk. Independently, give the backup bucket a versioned, delete-restricted policy — that stops destruction regardless of who holds the key.

## A nameless instance serves its web UI in the clear

An instance deployed without a domain has no certificate, so pull requests, review, and login travel unencrypted. Git over SSH is encrypted regardless, so pushing to one across the internet is safe — it is the browser session that is exposed.

**What to do about it:** keep nameless instances on a LAN, a VPN, or a tailnet. Attach a domain when the instance outlives the experiment: `farrier attach` is an in-place operation that loses nothing but clone URLs, and it reports the ones that changed.

## A bundle initialized against a staging CA serves an untrusted certificate

`init -acme-directory staging` issues from Let's Encrypt's staging environment, whose root no browser and no git client trusts. That is the point — staging exists so the named tier can be rehearsed without spending production's rate limits — but the instance it produces is a rehearsal, not a soft launch. The choice is frozen in the manifest the way the domain and the image digests are, so every later `up` renews against staging too; there is no flag that promotes the bundle later.

**What to do about it:** treat a staging bundle as disposable. Rehearse with it, learn what breaks, then run `init` again without the flag for the instance the team will actually use. Check `acme.directoryUrl` in `farrier.yaml` if you are unsure which kind of bundle you are holding.

## The local API binds loopback

The control API listens on `127.0.0.1` by default and exposes a verb for every operation, including destructive ones. It has no authentication of its own — the loopback bind is the boundary.

Exposing it beyond loopback, through a VPN or a tailnet, is operator topology and operator responsibility. Anything that can reach that port can promote, restore, or upgrade your instance.

## Fork pull requests

On a public repository, a fork PR is the one place outside code reaches your CI. Forgejo requires maintainer approval before those workflows run, unconditionally once Actions is enabled — which the bundle does by default, so there is nothing to configure and no way to loosen it.

Private repositories, the target case, do not present this surface at all.

## Key material never rotates by default

Once `init` writes a piece of key material, nothing silently overwrites it — the same guarantee that keeps a second `init` from clobbering a live instance's identity. The TLS certificate and its private key are the only declared exception, because an ACME certificate must rotate before it expires.

The practical consequence: **there is no rotation command.** If your Forgejo `SECRET_KEY` or SSH host key is exposed, replacing it is a deliberate operation you perform against the keystore, and it invalidates what depends on it — sessions and stored secrets for `SECRET_KEY`, every client's `known_hosts` entry for the host key.

## Explicitly out of scope

- **Isolation against deliberately hostile code** beyond the container level. See the CI section above.
- **Multi-tenancy.** The instance assumes one team and one administrator. There is no tenant boundary to enforce.
- **DNS provider compromise or outage.** DNS is the failover path's single external dependency and sits outside what the system covers.
- **Host hardening.** Farrier requires Docker and SSH and touches nothing else. Firewalls, patching, SSH policy, and intrusion detection on that host are yours.
