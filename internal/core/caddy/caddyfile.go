// Package caddy renders Caddy's own configuration for a Farrier bundle: the
// Caddyfile that terminates TLS for the bundle domain with a core-issued
// certificate and reverse-proxies to the forge (UP-002).
//
// spec.md "What it's built on": "TLS termination: Caddy, as a dumb
// terminator. The core owns ACME and hands Caddy its certificates." Caddy
// never requests its own certificate here — the rendered Caddyfile always
// carries an explicit tls directive pointing at CertPath/KeyPath, never
// Caddy's own automatic HTTPS, which would make Caddy its own ACME client
// and duplicate the core's.
package caddy

import (
	"fmt"
	"strings"
)

// Service is the Compose service name of the bundle's TLS terminator —
// deploy.Up mounts the rendered Caddyfile and certificate under this
// service and publishes its HTTPS port (UP-002).
const Service = "caddy"

// Container-side layout of the official Caddy Docker image, plus the paths
// deploy.Up ships the core-issued certificate and key under. Fixed, not
// manifest-derived, the same way forge's paths are (forge/appini.go): a
// caller that deploys RenderCaddyfile's output must mount the certificate
// and key here.
const (
	ConfigPath = "/etc/caddy/Caddyfile"
	CertPath   = "/etc/caddy/certs/tls.crt"
	KeyPath    = "/etc/caddy/certs/tls.key"
)

// RenderCaddyfile renders a Caddyfile that terminates TLS for domain with
// the certificate and key at CertPath/KeyPath and reverse-proxies every
// request to upstream — typically forge.Service plus forge.HTTPPort, the
// address the shared Compose network resolves it at.
func RenderCaddyfile(domain, upstream string) ([]byte, error) {
	domain = strings.TrimSpace(domain)
	upstream = strings.TrimSpace(upstream)
	if domain == "" {
		return nil, fmt.Errorf("caddy: domain is required")
	}
	if upstream == "" {
		return nil, fmt.Errorf("caddy: upstream is required")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s {\n", domain)
	fmt.Fprintf(&b, "\ttls %s %s\n", CertPath, KeyPath)
	fmt.Fprintf(&b, "\treverse_proxy %s\n", upstream)
	fmt.Fprintf(&b, "}\n")
	return []byte(b.String()), nil
}

// RenderPlainHTTPCaddyfile renders a Caddyfile that serves address over
// plain HTTP and reverse-proxies every request to upstream — what a
// nameless bundle gets (UP-006), where there is no domain to name a
// certificate for and the operator supplies the address at `up`.
//
// The `http://` prefix on the site address is load-bearing rather than
// decorative. A bare site address puts Caddy into automatic HTTPS: it would
// listen on 443, and it would try to obtain its own certificate — becoming
// the second ACME client this package's doc comment exists to rule out, for
// a name no ACME server would issue for anyway. Prefixed, the site is
// HTTP-only on port 80 and Caddy attempts no issuance at all.
//
// address is an IP or a hostname, spelled for a URL authority (an IPv6
// literal bracketed). It is the same string the caller renders into
// Forgejo's ROOT_URL, so the site Caddy answers for and the URL the forge
// tells browsers about are the same one.
//
// What this costs the operator is stated in spec.md "Instances without a
// name" and docs/security.md "A nameless instance serves its web UI in the
// clear": pull requests, review, and login travel unencrypted, so the
// instance belongs on a LAN, a VPN, or a tailnet. Git over SSH is
// untouched by any of this and is encrypted regardless.
func RenderPlainHTTPCaddyfile(address, upstream string) ([]byte, error) {
	address = strings.TrimSpace(address)
	upstream = strings.TrimSpace(upstream)
	if address == "" {
		return nil, fmt.Errorf("caddy: address is required")
	}
	if upstream == "" {
		return nil, fmt.Errorf("caddy: upstream is required")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "http://%s {\n", address)
	fmt.Fprintf(&b, "\treverse_proxy %s\n", upstream)
	fmt.Fprintf(&b, "}\n")
	return []byte(b.String()), nil
}

// RenderPushHoldCaddyfile renders the same TLS-terminating,
// reverse-proxying Caddyfile RenderCaddyfile does, with two routes ahead of
// the proxy that reject git's smart-HTTP push endpoints —
// `POST .../git-receive-pack` and `GET .../info/refs?service=git-receive-pack`
// — with a 503 and message, instead of forwarding them on. Every other
// request, including fetches and clones, reaches upstream unchanged
// (BKUP-002, spec.md "Backups": reads and fetches stay live; only pushes
// are held).
//
// backup.CaddyPushHold reloads Caddy against this rendered config for the
// duration of the hold, then reloads back to the original Caddyfile at
// ConfigPath to release it — the reject is a clean, immediate failure, not
// a queued or buffered request, so a client mid-push simply gets an error
// and retries.
func RenderPushHoldCaddyfile(domain, upstream, message string) ([]byte, error) {
	domain = strings.TrimSpace(domain)
	upstream = strings.TrimSpace(upstream)
	message = strings.TrimSpace(message)
	if domain == "" {
		return nil, fmt.Errorf("caddy: domain is required")
	}
	if upstream == "" {
		return nil, fmt.Errorf("caddy: upstream is required")
	}
	if message == "" {
		return nil, fmt.Errorf("caddy: message is required")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s {\n", domain)
	fmt.Fprintf(&b, "\ttls %s %s\n", CertPath, KeyPath)
	fmt.Fprintf(&b, "\t@git_push {\n")
	fmt.Fprintf(&b, "\t\tmethod POST\n")
	fmt.Fprintf(&b, "\t\tpath */git-receive-pack\n")
	fmt.Fprintf(&b, "\t}\n")
	fmt.Fprintf(&b, "\t@git_push_refs {\n")
	fmt.Fprintf(&b, "\t\tmethod GET\n")
	fmt.Fprintf(&b, "\t\tpath */info/refs\n")
	fmt.Fprintf(&b, "\t\tquery service=git-receive-pack\n")
	fmt.Fprintf(&b, "\t}\n")
	fmt.Fprintf(&b, "\thandle @git_push {\n")
	fmt.Fprintf(&b, "\t\trespond %q 503\n", message)
	fmt.Fprintf(&b, "\t}\n")
	fmt.Fprintf(&b, "\thandle @git_push_refs {\n")
	fmt.Fprintf(&b, "\t\trespond %q 503\n", message)
	fmt.Fprintf(&b, "\t}\n")
	fmt.Fprintf(&b, "\treverse_proxy %s\n", upstream)
	fmt.Fprintf(&b, "}\n")
	return []byte(b.String()), nil
}
