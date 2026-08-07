package orchestrate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// DefaultTimeout bounds the dial and handshake when Options.Timeout is zero.
const DefaultTimeout = 15 * time.Second

// Options configures how Connect authenticates and verifies a host.
type Options struct {
	// KeyFile, given, authenticates with this private key instead of the
	// operator's SSH agent.
	KeyFile string
	// KnownHostsFile overrides the known_hosts path used to verify host
	// identity. Empty uses ~/.ssh/known_hosts.
	KnownHostsFile string
	// Timeout bounds the dial and handshake. Zero uses DefaultTimeout.
	Timeout time.Duration
}

// Client is a connected SSH session to one operator host.
type Client struct {
	target Target
	conn   *ssh.Client
}

// Connect reaches target over SSH, authenticating with the operator's SSH
// agent or an explicit key file (Options.KeyFile) and verifying the host
// against known_hosts. This — plus Docker, checked separately by
// CheckHost — is the whole of what ORCH-001 requires the host to provide.
func Connect(ctx context.Context, raw string, opts Options) (*Client, error) {
	target, err := ParseTarget(raw)
	if err != nil {
		return nil, err
	}

	auth, closer, err := authMethod(opts)
	if err != nil {
		return nil, err
	}
	defer closer.Close()

	hostKeyCB, err := hostKeyCallback(opts)
	if err != nil {
		return nil, err
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	rawConn, err := d.DialContext(dialCtx, "tcp", target.addr())
	if err != nil {
		return nil, fmt.Errorf("orchestrate: dial %s: %w", target.addr(), err)
	}

	if err := rawConn.SetDeadline(time.Now().Add(timeout)); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("orchestrate: dial %s: %w", target.addr(), err)
	}

	config := &ssh.ClientConfig{
		User:            target.User,
		Auth:            []ssh.AuthMethod{auth},
		HostKeyCallback: hostKeyCB,
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, target.addr(), config)
	if err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("orchestrate: connect to %s: %w", target, err)
	}
	if err := rawConn.SetDeadline(time.Time{}); err != nil {
		sshConn.Close()
		return nil, fmt.Errorf("orchestrate: connect to %s: %w", target, err)
	}

	return &Client{target: target, conn: ssh.NewClient(sshConn, chans, reqs)}, nil
}

// Target reports the host this client is connected to.
func (c *Client) Target() Target {
	return c.target
}

// Close ends the SSH connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Run executes command on the host in a fresh SSH session, streaming its
// stdout and stderr to the given writers (either may be nil to discard).
// Canceling ctx closes the session so the command's exit wait fails
// instead of leaving the process running unattended on the host.
func (c *Client) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	session, err := c.conn.NewSession()
	if err != nil {
		return fmt.Errorf("orchestrate: %s: open session: %w", c.target, err)
	}
	defer session.Close()

	if stdout != nil {
		session.Stdout = stdout
	}
	if stderr != nil {
		session.Stderr = stderr
	}

	if err := session.Start(command); err != nil {
		return fmt.Errorf("orchestrate: %s: start %q: %w", c.target, command, err)
	}

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	select {
	case <-ctx.Done():
		session.Close()
		<-done
		return fmt.Errorf("orchestrate: %s: %q: %w", c.target, command, ctx.Err())
	case err := <-done:
		if err != nil {
			return fmt.Errorf("orchestrate: %s: %q: %w", c.target, command, err)
		}
		return nil
	}
}

// CheckHost verifies the host meets the whole of what ORCH-001 requires
// beyond SSH itself: Docker, reachable over the same SSH session used for
// everything else. Its error names Docker specifically — the only thing
// Farrier ever requires of the host besides SSH — so an operator whose
// host lacks it gets a diagnosis instead of a generic exit-status failure.
func (c *Client) CheckHost(ctx context.Context) error {
	var stderr bytes.Buffer
	if err := c.Run(ctx, "docker version --format '{{.Server.Version}}'", io.Discard, &stderr); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("orchestrate: %s: Docker not usable over SSH: %s", c.target, msg)
		}
		return fmt.Errorf("orchestrate: %s: Docker not usable over SSH: %w", c.target, err)
	}
	return nil
}
