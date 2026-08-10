# Branch: c/exciting-tesla-rvj0kf

Fixes a deployment defect in **UP-002** and **UP-006**, and carries the
consequences through **UP-005**'s sibling field and **UP-007**'s rename
report.

## The defect

`up` publishes Caddy's host port unconditionally: `80:80` for a nameless
bundle, `443:443` for a named one. On a host already serving something on
that port, Docker refuses the deployment outright:

```
Error response from daemon: driver failed programming external connectivity
on endpoint farrier-caddy: Bind for 0.0.0.0:80 failed: port is already
allocated
```

Nothing in the design earns the assumption that Farrier's Caddy owns the
host's standard web ports. The operator brings the host. Port 80 in
particular is the most contended port on a developer's laptop, which is
exactly where the nameless tier (INIT-005, UP-006) is meant to be tried.

Only the *host* side of the mapping is contended — Caddy binds inside its
own network namespace, so container ports are untouched.

## The model

Two things are currently one number, and conflating them is the whole
defect:

- **the published host port** — where Caddy listens on the host
- **the public URL** — what `ROOT_URL`, clone URLs, and runner registration
  must say, because it is what clients actually connect to

They agree when Farrier's Caddy is the edge, and differ only when something
else on the host holds the standard port and forwards to Farrier.

| | published | public URL |
|---|---|---|
| nameless, default | 8222 | `http://<address>:8222` |
| named, default | 443 | `https://<domain>` |
| named, moved to 8443, no proxy | 8443 | `https://<domain>:8443` |
| named, moved to 8443, proxy on 443 | 8443 | `https://<domain>` |

So: the published port becomes a manifest field alongside `gitSshPort`, the
public URL is derived from domain-or-address plus the port (omitted when it
is the scheme's default), and a second field lets the operator assert the
public port when a proxy fronts the instance.

## The constraint that comes with fronting

Farrier owns the certificate: identity lives in the bundle and Caddy is the
sole TLS terminator. A fronting proxy must therefore pass TCP through (SNI
routing) and let Farrier terminate. A proxy that terminates TLS itself
breaks the identity model — the certificate clients see is no longer the
bundle's, and restore and promote stop delivering the unchanged-TLS-identity
guarantee they promise. That is a different product, not a configuration
option, and it gets written down next to the TLS decision in spec.md.
