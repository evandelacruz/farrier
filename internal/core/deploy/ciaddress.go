package deploy

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
)

// hostAddressCommand asks the host to list the addresses its own interfaces
// hold. Two tools, because the hosts `up` supports do not share one: `ip`
// is what a modern Linux distribution ships and `ifconfig` is what macOS
// does, and a deployment to the operator's own laptop is the case this
// question exists to answer. Whichever runs first and succeeds wins; if
// neither is there the command fails and the caller says so rather than
// inventing an address.
const hostAddressCommand = "ip -o addr show 2>/dev/null || ifconfig 2>/dev/null"

// hostAddressSuggestions bounds how many discovered addresses a refusal
// prints. A host with a dozen interfaces would otherwise answer "which
// address?" with a wall of them, which is the same as not answering.
const hostAddressSuggestions = 3

// virtualInterfacePrefixes are the interface names whose addresses are not
// an answer to "where do other machines reach this host". They are the
// container and virtual-machine bridges the host manages itself: an address
// on one is reachable from something on this host and from nowhere else, so
// suggesting it would swap one unreachable address for another that merely
// fails less obviously.
//
// Tunnel interfaces are deliberately absent from the list. A tailnet
// address lives on one, and spec.md "Instances without a name" calls it the
// best of the nameless options — stable, private, reachable from anywhere
// the operator is logged in — so it is the answer this refusal most wants
// to be able to give.
var virtualInterfacePrefixes = []string{"lo", "docker", "br-", "veth", "virbr", "bridge", "vmnet"}

// checkRunnerReachableAddress refuses a deployment whose web address is a
// loopback address when that deployment also carries a colocated CI runner
// (FORGE-005, UP-006).
//
// The address is not only what the operator's browser uses. It becomes
// Forgejo's ROOT_URL, and the runner hands ROOT_URL's host to every job
// container as the server to clone from; it is also the address the runner
// daemon itself registers against and reaches for artifacts. A loopback
// address is the container's own loopback inside every one of those, so a
// deployment made with one comes up entirely healthy and cannot run a
// single workflow — the runner restarts forever against its own port 8222
// and the first job queues with no error anywhere an operator looks. That
// silence is why this is a refusal rather than a warning.
//
// It applies only when a colocated runner is actually being deployed.
// A browse-only instance on the operator's own machine has no container
// that has to reach it, and `-address 127.0.0.1` is a legitimate way to run
// one, so a bundle with the runner turned off is left alone.
//
// The refusal names a concrete address the host answers on (XCUT-003: what
// failed, why, and what to do). Deciding needs nothing from the host —
// whether an address is loopback is answerable on the operator's machine —
// so the host is asked only on the way to the error, and a host that cannot
// answer still gets refused, with the requirement stated instead of an
// address.
func checkRunnerReachableAddress(ctx context.Context, host Host, m *bundle.Manifest, address string) error {
	if address == "" || !colocatedRunnerPlanned(m) || !isLoopbackAddress(address) {
		return nil
	}
	return fmt.Errorf("deploy: %s is a loopback address, and this deployment carries a CI runner: %s. %s",
		address, loopbackConsequence, addressAdvice(hostAddresses(ctx, host)))
}

// loopbackConsequence is what a loopback address does to CI, in the terms
// an operator can check for themselves. It is the "why" half of the
// refusal, and it is stated in full because the failure it prevents is
// invisible — nothing in the event stream, the forge's admin UI, or the
// queued workflow says what is wrong.
const loopbackConsequence = "the forge tells CI job containers to clone from this address, and inside a container it is the container itself, so every workflow fails with no error on the instance"

// addressAdvice is the "what to do" half: an address the host actually
// answers on, or — when the host could not be asked, or holds nothing but
// loopback and virtual interfaces — the requirement itself, so the operator
// is never left with a refusal and no next step.
func addressAdvice(addrs []string) string {
	if len(addrs) == 0 {
		return "re-run with an address other machines on your network reach this host at — asking the host for one turned up nothing — or turn the runner off with colocatedRunner: false in " + bundle.ManifestFile + " and register a remote runner instead"
	}
	if len(addrs) > hostAddressSuggestions {
		addrs = addrs[:hostAddressSuggestions]
	}
	return fmt.Sprintf("this host is reachable at %s — re-run with one of those as the address, or turn the runner off with colocatedRunner: false in %s and register a remote runner instead",
		strings.Join(addrs, ", "), bundle.ManifestFile)
}

// isLoopbackAddress reports whether address — already normalized for a URL
// authority (NormalizeAddress), so an IPv6 literal arrives bracketed — names
// the machine it is evaluated on rather than a machine on a network.
//
// Three spellings, not one: the whole of 127.0.0.0/8 rather than the
// familiar 127.0.0.1, `::1` for IPv6, and the name `localhost`. RFC 6761
// reserves `localhost` and every name under it for exactly this, so
// `foo.localhost` counts too — and a hostname is compared case-insensitively
// because DNS names are.
func isLoopbackAddress(address string) bool {
	address = strings.TrimSpace(address)
	if address == "" {
		return false
	}
	if ip := net.ParseIP(strings.Trim(address, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	name := strings.ToLower(address)
	return name == "localhost" || strings.HasSuffix(name, ".localhost")
}

// hostAddresses asks the host for the addresses it holds and returns the
// ones another machine could reach it at, IPv4 first: an operator reads an
// IPv4 literal off a router's admin page and types it back, and a host with
// both is far likelier to be reached at the v4 one.
//
// A failure is not one: this runs only on the way to an error that is being
// returned regardless, and the caller reports "none discovered" rather than
// letting a missing `ip` binary replace a useful message with a different
// one about the tool that was meant to produce it.
func hostAddresses(ctx context.Context, host Host) []string {
	out, err := host.Output(ctx, hostAddressCommand)
	if err != nil {
		return nil
	}
	return parseHostAddresses(string(out))
}

// parseHostAddresses pulls routable addresses out of what
// hostAddressCommand printed, in either tool's format.
//
// The two agree on more than they disagree on: both name the interface at
// the start of a line that does not begin with whitespace, and both write
// an address as the field after an `inet` or `inet6` marker. `ip -o` numbers
// its lines ("2: eth0    inet 192.168.1.5/24 ...") and ifconfig does not
// ("en0: flags=8863<UP,...>"), which is the one branch below. Suffixes
// differ too — a prefix length on Linux, a zone on an IPv6 link-local
// address — and both are cut before parsing.
//
// An address whose interface could not be determined is kept rather than
// dropped. This is a best-effort suggestion inside a message that is useful
// without it, so the failure mode worth avoiding is silence.
func parseHostAddresses(out string) []string {
	var v4, v6 []string
	seen := make(map[string]bool)
	iface := ""

	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && strings.HasSuffix(fields[0], ":") {
			// `ip -o` prefixes an index; ifconfig starts with the name.
			if name := strings.TrimSuffix(fields[0], ":"); isAllDigits(name) && len(fields) > 1 {
				iface = fields[1]
			} else {
				iface = name
			}
		}
		if isVirtualInterface(iface) {
			continue
		}
		for i, field := range fields {
			if field != "inet" && field != "inet6" || i+1 >= len(fields) {
				continue
			}
			literal, _, _ := strings.Cut(fields[i+1], "/")
			literal, _, _ = strings.Cut(literal, "%")
			ip := net.ParseIP(literal)
			if ip == nil || !routableAddress(ip) || seen[literal] {
				continue
			}
			seen[literal] = true
			if ip.To4() != nil {
				v4 = append(v4, literal)
			} else {
				// Bracketed, because that is how it would be passed back
				// to `-address` and rendered into every URL that follows
				// (NormalizeAddress).
				v6 = append(v6, "["+literal+"]")
			}
		}
	}
	return append(v4, v6...)
}

// routableAddress reports whether ip is one another machine could plausibly
// reach this host at. Loopback is the case being refused; link-local and
// unspecified addresses are not addresses a client can be handed; multicast
// is not a host at all.
func routableAddress(ip net.IP) bool {
	return !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() &&
		!ip.IsUnspecified() && !ip.IsMulticast()
}

// isVirtualInterface reports whether name is one of the host's own
// container or VM bridges. An empty name — an output format neither branch
// of parseHostAddresses recognized — is not one: see that function's last
// paragraph.
func isVirtualInterface(name string) bool {
	if name == "" {
		return false
	}
	for _, prefix := range virtualInterfacePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
