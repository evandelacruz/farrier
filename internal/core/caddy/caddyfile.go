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
