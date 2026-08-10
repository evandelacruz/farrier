package deploy

import (
	"fmt"
	"net"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
)

// maxHostnameLength and maxLabelLength are the DNS limits NormalizeAddress
// holds a hostname to, so an address that could never resolve is rejected
// on the operator's machine rather than after Caddy has been handed a site
// block it will refuse to load.
const (
	maxHostnameLength = 253
	maxLabelLength    = 63
)

// serveAddress resolves the address this deployment serves the forge's web
// UI at, from the bundle and the operator's Options.Address (UP-006).
//
// The two are mutually exclusive, and that is the whole of the rule:
//
//   - A named bundle (INIT-005's opposite) carries its own address. Its
//     domain is the identity every URL derives from and `up` completes with
//     HTTPS at it (UP-002), so an operator-supplied address would be a
//     second, conflicting answer to a question the bundle already settles.
//     Supplying one fails rather than being quietly ignored.
//   - A nameless bundle carries none. It has no domain, no certificate, and
//     nothing to render a ROOT_URL from, so `up` is where the instance
//     learns how it is reached (spec.md "Instances without a name": "`up`
//     serves it over plain HTTP at whatever address the operator gives").
//     Omitting the address fails, naming the flag, before the deployment
//     touches the host.
//
// The returned address is empty for a named bundle — the caller reads
// Manifest.Named() to know which of the two it is holding — and, for a
// nameless one, is spelled the way a URL authority wants it: unchanged for
// a hostname or an IPv4 literal, bracketed for an IPv6 one.
func serveAddress(m *bundle.Manifest, address string) (string, error) {
	address = strings.TrimSpace(address)
	if m.Named() {
		if address != "" {
			return "", fmt.Errorf("deploy: this bundle is named %s and up serves it over HTTPS at that domain (UP-002); an address is for a nameless bundle only (UP-006)", strings.TrimSpace(m.Domain))
		}
		return "", nil
	}
	if address == "" {
		return "", fmt.Errorf("deploy: this bundle is nameless (INIT-005), so up needs the address — an IP or a hostname — to serve its web UI at (UP-006)")
	}
	return NormalizeAddress(address)
}

// NormalizeAddress checks that address is an IP literal or a hostname and
// returns it spelled for the authority component of a URL: an IPv6 literal
// comes back bracketed, everything else unchanged.
//
// Exported because `up` is no longer the only operation that takes a
// nameless instance's address: attaching a name to one (UP-007) reports the
// clone URLs that address used to serve, and it has to spell them the way
// `up` spelled them or the "was" half of that report names URLs nobody ever
// used. One grammar, one normalization, one spelling.
//
// It is deliberately strict about what it is *not*. The address is used
// verbatim as Caddy's site address, Forgejo's DOMAIN, SSH_DOMAIN, and the
// host of ROOT_URL, so a scheme, a port, a path, or a userinfo prefix would
// each produce a different kind of quietly broken instance — a ROOT_URL of
// `http://http://box/`, a site block Caddy will not load, a clone URL
// nothing answers on. Rejecting them here, with a message naming the part
// that does not belong, is the only point where the operator can still fix
// it cheaply.
//
// The port in particular is not an oversight: `up` publishes the nameless
// instance's web UI on port 80 (see plainHTTPPort), the same way it
// publishes a named one's on 443, so there is no port for the operator to
// choose and an address carrying one would be silently ignored.
func NormalizeAddress(address string) (string, error) {
	switch {
	case strings.Contains(address, "://"):
		return "", fmt.Errorf("deploy: address %q carries a scheme; give the bare IP or hostname — up serves a nameless instance over plain HTTP (UP-006)", address)
	case strings.ContainsAny(address, "/?#"):
		return "", fmt.Errorf("deploy: address %q carries a path; give the bare IP or hostname", address)
	case strings.Contains(address, "@"):
		return "", fmt.Errorf("deploy: address %q carries a user; give the bare IP or hostname", address)
	case strings.ContainsAny(address, " \t\r\n"):
		return "", fmt.Errorf("deploy: address %q contains whitespace", address)
	}

	// An IP literal, in either family. IPv6 comes back bracketed because
	// every place this address lands — ROOT_URL, the Caddy site address,
	// the SSH clone URL — is a URL authority, where an unbracketed IPv6
	// literal is unparseable.
	if ip := net.ParseIP(address); ip != nil {
		if ip.To4() == nil {
			return "[" + address + "]", nil
		}
		return address, nil
	}
	// An already-bracketed IPv6 literal, which is how an operator who
	// copied the address out of a URL would spell it.
	if strings.HasPrefix(address, "[") && strings.HasSuffix(address, "]") {
		inner := address[1 : len(address)-1]
		if ip := net.ParseIP(inner); ip != nil && ip.To4() == nil {
			return address, nil
		}
		return "", fmt.Errorf("deploy: address %q is not a valid IPv6 literal", address)
	}

	if err := validHostname(address); err != nil {
		return "", err
	}
	return address, nil
}

// validHostname reports whether address is a syntactically valid DNS
// hostname: one or more dot-separated labels of letters, digits, and
// hyphens, no label starting or ending with a hyphen. A trailing dot is
// rejected rather than trimmed — it would survive into ROOT_URL and the
// clone URL, and an operator who typed one meant a fully-qualified name
// they can spell without it.
func validHostname(address string) error {
	if len(address) > maxHostnameLength {
		return fmt.Errorf("deploy: address %q is longer than %d characters", address, maxHostnameLength)
	}
	if strings.Contains(address, ":") {
		return fmt.Errorf("deploy: address %q carries a port; up publishes a nameless instance's web UI on port %s (UP-006)", address, plainHTTPPort)
	}
	for _, label := range strings.Split(address, ".") {
		if label == "" {
			return fmt.Errorf("deploy: address %q has an empty label", address)
		}
		if len(label) > maxLabelLength {
			return fmt.Errorf("deploy: address %q has a label longer than %d characters", address, maxLabelLength)
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("deploy: address %q has a label starting or ending with a hyphen", address)
		}
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			default:
				return fmt.Errorf("deploy: address %q is not an IP or a hostname", address)
			}
		}
	}
	return nil
}
