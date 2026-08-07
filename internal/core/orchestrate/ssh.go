package orchestrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

const defaultSSHPort = "22"

// SSHConfig configures a connection to a single host (ORCH-001). Nothing
// beyond Docker and SSH is required on the host; on the control-plane side,
// authentication and host-identity checking reuse whatever the operator
// already has set up for the `ssh` CLI, rather than asking them to manage a
// second identity story.
type SSHConfig struct {
	// Target is "ssh://user@host" or "ssh://user@host:port"; port defaults
	// to 22.
	Target string
	// KeyPath is an optional path to an unencrypted private key file. If
	// empty, Dial authenticates through the operator's SSH agent at
	// SSH_AUTH_SOCK.
	KeyPath string
	// KnownHostsPath is an optional path to a known_hosts file. If empty,
	// Dial uses ~/.ssh/known_hosts, same as the operator's own SSH client.
	KnownHostsPath string
	// Timeout bounds the initial connection. Zero means 30 seconds.
	Timeout time.Duration
}

// SSHTransport is a Transport backed by a real SSH connection (ORCH-001).
type SSHTransport struct {
	client *ssh.Client
}

// DialSSH connects to cfg.Target and returns a ready Transport.
func DialSSH(ctx context.Context, cfg SSHConfig) (*SSHTransport, error) {
	user, addr, err := parseTarget(cfg.Target)
	if err != nil {
		return nil, err
	}

	auth, agentConn, err := authMethod(cfg.KeyPath)
	if err != nil {
		return nil, err
	}
	if agentConn != nil {
		defer agentConn.Close()
	}

	hostKeyCallback, err := hostKeyCallback(cfg.KnownHostsPath)
	if err != nil {
		return nil, err
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	clientConfig := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{auth},
		HostKeyCallback: hostKeyCallback,
		Timeout:         timeout,
	}

	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("orchestrate: ssh: dial %s: %w", addr, err)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, clientConfig)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("orchestrate: ssh: handshake %s: %w", addr, err)
	}

	return &SSHTransport{client: ssh.NewClient(sshConn, chans, reqs)}, nil
}

// parseTarget splits a "ssh://user@host[:port]" target into an SSH user
// and a dial address, defaulting the port to 22.
func parseTarget(target string) (user, addr string, err error) {
	if strings.TrimSpace(target) == "" {
		return "", "", errors.New("orchestrate: ssh: target is required")
	}
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "ssh" || u.Hostname() == "" || u.User == nil || u.User.Username() == "" {
		return "", "", fmt.Errorf("orchestrate: ssh: target must be ssh://user@host[:port], got %q", target)
	}
	port := u.Port()
	if port == "" {
		port = defaultSSHPort
	}
	return u.User.Username(), net.JoinHostPort(u.Hostname(), port), nil
}

// authMethod resolves the operator's identity: an explicit key file if
// keyPath is set, otherwise the SSH agent at SSH_AUTH_SOCK. The returned
// closer, if non-nil, is the agent connection and must be closed once the
// handshake that consumes it has completed.
func authMethod(keyPath string) (ssh.AuthMethod, io.Closer, error) {
	if keyPath != "" {
		key, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, nil, fmt.Errorf("orchestrate: ssh: read key %s: %w", keyPath, err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, nil, fmt.Errorf("orchestrate: ssh: parse key %s: %w", keyPath, err)
		}
		return ssh.PublicKeys(signer), nil, nil
	}

	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, nil, errors.New("orchestrate: ssh: no key file configured and SSH_AUTH_SOCK is not set")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, nil, fmt.Errorf("orchestrate: ssh: connect to agent at %s: %w", sock, err)
	}
	agentClient := agent.NewClient(conn)
	return ssh.PublicKeysCallback(agentClient.Signers), conn, nil
}

// hostKeyCallback loads known_hosts (the operator's own by default) and
// verifies the host key against it, the same check the `ssh` CLI performs.
func hostKeyCallback(path string) (ssh.HostKeyCallback, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("orchestrate: ssh: locate known_hosts: %w", err)
		}
		path = filepath.Join(home, ".ssh", "known_hosts")
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("orchestrate: ssh: load known_hosts %s: %w", path, err)
	}
	return cb, nil
}

// Run executes command in a shell on the host and returns its stdout. A
// nonzero exit status is an error that includes stderr.
func (t *SSHTransport) Run(ctx context.Context, command string) ([]byte, error) {
	session, err := t.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("orchestrate: ssh: run: open session: %w", err)
	}
	defer session.Close()
	defer watchCancel(ctx, session)()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	if err := session.Run(command); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("orchestrate: ssh: run %q: %w", command, ctx.Err())
		}
		return nil, fmt.Errorf("orchestrate: ssh: run %q: %w%s", command, err, stderrSuffix(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// WriteFile writes content to remotePath on the host: it stages the write
// under remotePath+".tmp", sets mode, and moves it into place with a single
// mv, so a reader on the host never observes a partial write.
func (t *SSHTransport) WriteFile(ctx context.Context, remotePath string, content []byte, mode uint32) error {
	dir := path.Dir(remotePath)
	tmp := remotePath + ".tmp"
	command := fmt.Sprintf("mkdir -p %s && cat > %s && chmod %s %s && mv %s %s",
		shQuote(dir), shQuote(tmp), strconv.FormatUint(uint64(mode), 8), shQuote(tmp), shQuote(tmp), shQuote(remotePath))

	session, err := t.client.NewSession()
	if err != nil {
		return fmt.Errorf("orchestrate: ssh: write %s: open session: %w", remotePath, err)
	}
	defer session.Close()
	defer watchCancel(ctx, session)()

	session.Stdin = bytes.NewReader(content)
	var stderr bytes.Buffer
	session.Stderr = &stderr
	if err := session.Run(command); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("orchestrate: ssh: write %s: %w", remotePath, ctx.Err())
		}
		return fmt.Errorf("orchestrate: ssh: write %s: %w%s", remotePath, err, stderrSuffix(stderr.String()))
	}
	return nil
}

// Close releases the underlying SSH connection.
func (t *SSHTransport) Close() error {
	return t.client.Close()
}

// watchCancel closes session as soon as ctx is done, so a command actually
// stops running on the host when the caller's context is canceled instead
// of leaking a session past it. The returned func stops the watch and must
// be called (via defer, immediately after) once the session-owning call has
// returned.
func watchCancel(ctx context.Context, session *ssh.Session) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			session.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func stderrSuffix(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return fmt.Sprintf(" (stderr: %s)", stderr)
}
