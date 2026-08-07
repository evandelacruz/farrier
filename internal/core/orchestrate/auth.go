package orchestrate

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// nopCloser is a no-op io.Closer for auth paths that own no resource.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// authMethod resolves how Connect authenticates: an explicit key file when
// opts.KeyFile is set, otherwise the operator's running SSH agent
// (SSH_AUTH_SOCK) — ORCH-001 names exactly these two paths and nothing
// else, so there is no fallback to scanning ~/.ssh for default keys or to
// an interactive password prompt.
//
// The returned io.Closer releases any resource (the agent socket) that
// only needs to live for the handshake; the caller closes it once Connect
// finishes, success or not.
func authMethod(opts Options) (ssh.AuthMethod, io.Closer, error) {
	if opts.KeyFile != "" {
		method, err := keyFileAuth(opts.KeyFile)
		return method, nopCloser{}, err
	}
	return agentAuth()
}

func keyFileAuth(path string) (ssh.AuthMethod, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("orchestrate: read key file %s: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("orchestrate: parse key file %s: %w", path, err)
	}
	return ssh.PublicKeys(signer), nil
}

func agentAuth() (ssh.AuthMethod, io.Closer, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, nil, errors.New("orchestrate: no SSH agent running (SSH_AUTH_SOCK not set) and no key file configured")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, nil, fmt.Errorf("orchestrate: connect to SSH agent at %s: %w", sock, err)
	}
	return ssh.PublicKeysCallback(agent.NewClient(conn).Signers), conn, nil
}
