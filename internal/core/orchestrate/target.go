// Package orchestrate is the SSH transport to operator hosts (ORCH-001) and,
// on top of it, Compose rendering and convergence (ORCH-002).
package orchestrate

import (
	"fmt"
	"net"
	"net/url"
)

// DefaultPort is used when a target omits one.
const DefaultPort = "22"

// Target is a parsed ssh://user@host[:port] address.
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
