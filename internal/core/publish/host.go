package publish

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/deploy"
)

// resolveHost picks the host `publish` addresses the instance at: the one
// the git remote URL names and the one the pinned known_hosts entry keys
// (IMPT-004). It is a single answer on purpose — two derivations would be
// two chances for the remote and the pin to disagree, and a disagreement
// there fails the push with a host-key error rather than a message anyone
// can act on.
//
// A named bundle answers it with its domain, which is what the identity in
// the bundle is for (spec.md "The domain"). A nameless bundle carries no
// name at all (INIT-005), so the operator's address takes that role
// (UP-006) and `publish` learns it the same way `up` did:
//
//   - Options.Address, when the operator gives one.
//   - otherwise the host of Options.TargetBaseURL, because the ordinary
//     case is one machine: `publish -target http://192.168.1.5:8222`
//     names the instance's API, and the forge answers git over SSH at that
//     same host on its own port.
//   - neither, and the publish fails naming both ways out before anything
//     is created — the posture deploy.serveAddress takes for `up`.
//
// The derived default is a convenience with an escape hatch, and the escape
// hatch is the point: the API can be reached through a tunnel or a proxy on
// a host that is not where SSH answers, which is the same reason the
// manifest keeps the port Caddy is published on apart from the port clients
// connect to. An operator in that position names the SSH host explicitly.
//
// An address supplied for a named bundle is rejected rather than ignored,
// the pairing deploy.serveAddress and forge.RenderAppINI already enforce:
// the bundle has already answered this, and a second answer is a
// disagreement, not an override.
//
// The returned host is spelled for a URL authority — unchanged for a
// hostname or an IPv4 literal, bracketed for an IPv6 one — because that is
// what bundle.Manifest.GitSSHCloneURLAt and PublicURLAt take.
func resolveHost(opts Options) (string, error) {
	address := strings.TrimSpace(opts.Address)
	if opts.Manifest.Named() {
		if address != "" {
			return "", fmt.Errorf("this bundle is named %s and publish reaches the instance at that domain; an address is for a nameless bundle only", strings.TrimSpace(opts.Manifest.Domain))
		}
		return strings.TrimSpace(opts.Manifest.Domain), nil
	}
	if address != "" {
		return deploy.NormalizeAddress(address)
	}

	target := strings.TrimSpace(opts.TargetBaseURL)
	if target == "" {
		return "", fmt.Errorf("this bundle is nameless, so publish needs the address you deployed the instance at: give one with -address, or name the instance's API with -target and publish uses its host")
	}
	host, err := hostOf(target)
	if err != nil {
		return "", err
	}
	return host, nil
}

// hostOf pulls the host out of an instance's API base URL, spelled for a
// URL authority.
//
// It is taken verbatim rather than run back through
// deploy.NormalizeAddress: it came out of a URL that parsed, so it is
// already spelled the way every place it lands wants it, and re-validating
// it against `up`'s hostname grammar would refuse an address the operator
// is demonstrably reaching the instance at.
func hostOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("target %q is not a URL: %w", raw, err)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("target %q names no host, so publish cannot tell where the instance answers git over SSH: give the address with -address", raw)
	}
	// url.Hostname strips the brackets an IPv6 literal arrives in, and
	// every place this host lands is a URL authority, where it needs them
	// back. A colon can only be an IPv6 literal here: the port is gone.
	if strings.Contains(host, ":") {
		return "[" + host + "]", nil
	}
	return host, nil
}
