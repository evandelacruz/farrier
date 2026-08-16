package orchestrate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"path"
	"strconv"
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

	auth, err := authMethod(opts)
	if err != nil {
		return nil, err
	}
	defer auth.closer.Close()

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
		Auth:            []ssh.AuthMethod{auth.method},
		HostKeyCallback: hostKeyCB,
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, target.addr(), config)
	if err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("orchestrate: connect to %s: %w%s", target, err, authFailureHint(auth, err))
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
	return c.run(ctx, command, nil, stdout, stderr)
}

// Output executes command on the host and returns its stdout. A nonzero
// exit status is an error whose message carries stderr, so a caller that
// discards the output still learns why the command failed.
//
// This is the shape Transport wants: Run streams to writers the caller
// supplies, which suits CheckHost, while callers that just need the bytes
// (Converge) would otherwise each build their own buffer.
func (c *Client) Output(ctx context.Context, command string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	if err := c.run(ctx, command, nil, &stdout, &stderr); err != nil {
		return nil, fmt.Errorf("%w%s", err, stderrSuffix(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// WriteFile writes content to remotePath on the host with mode as the final
// permissions, creating parent directories as needed.
//
// The write is atomic from a reader's point of view: content lands in a
// staging file alongside the target and is moved into place only once it is
// complete, so a concurrent reader on the host sees either the previous file
// or the whole new one, never a partial write. A failure anywhere in the
// chain leaves the target untouched.
//
// Content travels over the session's stdin rather than being embedded in the
// command, so it is never shell-quoted and never appears in the host's
// process list.
func (c *Client) WriteFile(ctx context.Context, remotePath string, content []byte, mode uint32) error {
	dir := path.Dir(remotePath)
	tmp := remotePath + ".tmp"
	command := fmt.Sprintf("mkdir -p %s && cat > %s && chmod %s %s && mv %s %s",
		shQuote(dir), shQuote(tmp), strconv.FormatUint(uint64(mode), 8),
		shQuote(tmp), shQuote(tmp), shQuote(remotePath))

	var stderr bytes.Buffer
	if err := c.run(ctx, command, bytes.NewReader(content), nil, &stderr); err != nil {
		return fmt.Errorf("orchestrate: %s: write %s: %w%s", c.target, remotePath, err, stderrSuffix(stderr.String()))
	}
	return nil
}

// run is the one place a session is opened, wired up, and waited on. Run,
// Output, and WriteFile differ only in what they attach to it.
//
// It is also the one place the docker PATH fallback is applied, and the one
// place a failure is turned into a diagnosis. Every command this package
// runs — and so every docker invocation and every remote write any package
// makes, since they all reach a host through this transport — goes over a
// session started here, which is what keeps both a single decision rather
// than one per caller.
//
// Diagnosing needs the host's own words, and the caller may have taken
// stderr for itself (Run streams it to a writer; Output and WriteFile
// buffer it to report separately). So stderr is tapped rather than
// intercepted: whoever asked for it still gets every byte, and run keeps a
// bounded copy to read the failure from.
func (c *Client) run(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	session, err := c.conn.NewSession()
	if err != nil {
		return fmt.Errorf("orchestrate: %s: open session: %w", c.target, err)
	}
	defer session.Close()

	if stdin != nil {
		session.Stdin = stdin
	}
	if stdout != nil {
		session.Stdout = stdout
	}
	var tap stderrTap
	if stderr != nil {
		session.Stderr = io.MultiWriter(stderr, &tap)
	} else {
		session.Stderr = &tap
	}

	if err := session.Start(withDockerPath(command)); err != nil {
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
			return fmt.Errorf("orchestrate: %s: %q: %w%s%s", c.target, command, err,
				dockerMissingHint(command, err), remoteDirHint(command, tap.String()))
		}
		return nil
	}
}

// stderrTapLimit bounds what a tap keeps. A refusal is one line; anything
// beyond this is a command that failed for some other reason and is being
// verbose about it, and holding all of it in memory to diagnose a failure
// that will not be diagnosed here buys nothing.
const stderrTapLimit = 4 << 10

// stderrTap keeps the first stderrTapLimit bytes a session writes to
// stderr and discards the rest. It never errors and never short-writes:
// its only reader is a hint, and a command whose diagnosis was truncated
// must still fail on its own terms rather than on this.
type stderrTap struct {
	buf []byte
}

func (t *stderrTap) Write(p []byte) (int, error) {
	if room := stderrTapLimit - len(t.buf); room > 0 {
		t.buf = append(t.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (t *stderrTap) String() string { return string(t.buf) }

// RunStdin executes command on the host in a fresh SSH session, streaming
// stdin to the process and its stdout and stderr to the given writers
// (either may be nil to discard). It is Run's counterpart for the opposite
// direction: Run lets a remote command's output stream to the caller (used
// to capture a tar archive, e.g. SSHGitCapturer.Archive); RunStdin lets the
// caller stream content into a remote command (used by restore.Restore to
// extract a decrypted snapshot's git archives and database file onto a
// host without holding either in memory). Canceling ctx closes the session
// the same way Run's does.
func (c *Client) RunStdin(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	return c.run(ctx, command, stdin, stdout, stderr)
}

// dockerVersionCommand asks the host's Docker daemon for its version. It is
// the readiness check: it fails if the CLI is missing and equally if the CLI
// is there but cannot reach a daemon.
const dockerVersionCommand = "docker version --format '{{.Server.Version}}'"

// CheckHost verifies the host meets the whole of what ORCH-001 requires
// beyond SSH itself: Docker, reachable over the same SSH session used for
// everything else. Its error names Docker specifically — the only thing
// Farrier ever requires of the host besides SSH — so an operator whose
// host lacks it gets a diagnosis instead of a generic exit-status failure.
// When the CLI itself could not be found, the diagnosis carries
// dockerMissingHint's explanation of where Farrier looked and why an SSH
// session sees a different PATH than a terminal does.
func (c *Client) CheckHost(ctx context.Context) error {
	var stderr bytes.Buffer
	if err := c.Run(ctx, dockerVersionCommand, io.Discard, &stderr); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			// Run's error already carries the hint; this branch drops it
			// in favor of the host's own stderr, so re-attach it here.
			return fmt.Errorf("orchestrate: %s: Docker not usable over SSH: %s%s",
				c.target, msg, dockerMissingHint(dockerVersionCommand, err))
		}
		return fmt.Errorf("orchestrate: %s: Docker not usable over SSH: %w", c.target, err)
	}
	return nil
}
