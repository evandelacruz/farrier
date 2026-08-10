// Package orchestrate is the SSH transport to operator hosts (ORCH-001) and,
// on top of it, Compose rendering and convergence (ORCH-002).
//
// # A host is a host
//
// ORCH-003, and spec.md "A host is a host": ssh://user@localhost and
// ssh://user@a-vps run the identical path. There is no local mode, no
// branch on locality, and nothing skipped because the host happens to be
// the operator's own machine. Locality is an argument, not a branch.
//
// Two properties of this package's shape are what make that true, rather
// than a rule anyone has to remember:
//
// Connect is the only door to a host. Nothing else in the tree parses an
// address or dials one, so there is exactly one place a locality test
// could be written.
//
// Nothing above the transport receives an address. Converge takes a
// Transport, deploy.Up takes a Host, the exporters take a state.Runner —
// all of them connections, none of them addresses. Code that cannot see
// where a host is cannot behave differently based on where it is. Target
// travels past Connect only as data an operation needs on its own terms
// (backup builds ssh:// remote URLs from it; upgrade records it in a
// recovery note), never as something to decide on.
//
// locality_test.go holds both halves of the proof: a real SSH connection
// addressed as localhost issues a byte-identical command transcript to one
// that believes it is talking to a VPS, and a guard over the tree fails if
// any product code starts comparing a host against a loopback name.
package orchestrate

import (
	"fmt"
	"net"
	"net/url"
)

// DefaultPort is used when a target omits one.
const DefaultPort = "22"

// Target is a parsed ssh://user@host[:port] address.
//
// Host is whatever the operator wrote. "localhost", "127.0.0.1", "::1" and
// "a-vps.example.com" are all just hosts here: ParseTarget applies the same
// rules to each, and every field below is derived the same way for all of
// them (ORCH-003).
type Target struct {
	User string
	Host string
	Port string
}

// String renders the target back to ssh://user@host:port form.
func (t Target) String() string {
	return fmt.Sprintf("ssh://%s@%s", t.User, net.JoinHostPort(t.Host, t.Port))
}

// ParseTarget parses an ssh://user@host[:port] address. A missing port
// defaults to 22. The scheme, user, and host are all required.
func ParseTarget(raw string) (Target, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Target{}, fmt.Errorf("orchestrate: parse target %q: %w", raw, err)
	}
	if u.Scheme != "ssh" {
		return Target{}, fmt.Errorf("orchestrate: parse target %q: scheme must be ssh://", raw)
	}
	if u.Hostname() == "" {
		return Target{}, fmt.Errorf("orchestrate: parse target %q: missing host", raw)
	}
	if u.User == nil || u.User.Username() == "" {
		return Target{}, fmt.Errorf("orchestrate: parse target %q: missing user (expected ssh://user@host)", raw)
	}

	port := u.Port()
	if port == "" {
		port = DefaultPort
	}
	return Target{
		User: u.User.Username(),
		Host: u.Hostname(),
		Port: port,
	}, nil
}

// addr is the host:port pair dialed for this target.
func (t Target) addr() string {
	return net.JoinHostPort(t.Host, t.Port)
}
