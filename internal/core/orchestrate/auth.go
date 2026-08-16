package orchestrate

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// sshKeyFlag is how an operator names a private key file on the command
// line. Every failure below that suggests naming one — or not naming one —
// spells it, because "give Farrier a key file" is not advice anybody can
// act on without knowing where to type it. It is a constant so the eight
// commands that define the flag and the messages that name it cannot drift
// apart.
const sshKeyFlag = "-ssh-key"

// agentListCommand is what an operator runs to see what their SSH agent is
// actually holding. Every message about agent authentication ends here: an
// operator who believes a key is loaded and a host that saw no such key
// disagree about one observable fact, and this is the command that settles
// it.
const agentListCommand = "ssh-add -l"

// authPaths describes, in one sentence, the whole of what Farrier will
// authenticate with. Both the agent path and the key-file path say it, and
// they say the same thing, because the failure they are explaining is
// almost always the operator expecting a third path that does not exist:
// a key file OpenSSH would have picked up from ~/.ssh on its own.
const authPaths = "Farrier authenticates with the key file you name with " + sshKeyFlag +
	", or, when you name none, with the keys your SSH agent is holding — nothing else, and it never prompts for a password. " +
	"A key sitting in ~/.ssh is not offered just for being on disk: load it with `ssh-add ~/.ssh/id_ed25519`, or name it with " +
	sshKeyFlag + ". `" + agentListCommand + "` lists what the agent is holding right now."

// nopCloser is a no-op io.Closer for auth paths that own no resource.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// authChoice is how Connect authenticated: the method it offered, the
// resource that only needs to live for the handshake, and a description of
// the credential in the operator's own terms.
//
// The description exists for the failure path. A host that refuses every
// key answers with "no supported methods remain" and nothing about what it
// was offered, so an operator reading that has no way to tell whether
// Farrier used the key file they meant or fell through to an agent holding
// something else. authFailureHint says which it was.
type authChoice struct {
	method ssh.AuthMethod
	closer io.Closer
	source string
}

// authMethod resolves how Connect authenticates: an explicit key file when
// opts.KeyFile is set, otherwise the operator's running SSH agent
// (SSH_AUTH_SOCK) — ORCH-001 names exactly these two paths and nothing
// else, so there is no fallback to scanning ~/.ssh for default keys or to
// an interactive password prompt.
//
// The returned io.Closer releases any resource (the agent socket) that
// only needs to live for the handshake; the caller closes it once Connect
// finishes, success or not.
func authMethod(opts Options) (authChoice, error) {
	if opts.KeyFile != "" {
		method, err := keyFileAuth(opts.KeyFile)
		if err != nil {
			return authChoice{}, err
		}
		return authChoice{
			method: method,
			closer: nopCloser{},
			source: fmt.Sprintf("the key in %s", opts.KeyFile),
		}, nil
	}
	return agentAuth()
}

// keyFileAuth loads the private key at path.
//
// Its two failure paths are the ones an operator actually hits, and each
// says what to do rather than only what went wrong. A passphrase-protected
// key is the sharper of the two: Farrier has no way to ask for the
// passphrase and never will, so the message routes the operator to the
// agent, which is the mechanism that exists for exactly this.
func keyFileAuth(path string) (ssh.AuthMethod, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("orchestrate: cannot read the SSH key file %s: %w. Give %s a path to a private key this user can read, or drop %s to use the keys your SSH agent is holding (`%s` lists them)",
			path, err, sshKeyFlag, sshKeyFlag, agentListCommand)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		var passphrase *ssh.PassphraseMissingError
		if errors.As(err, &passphrase) {
			return nil, fmt.Errorf("orchestrate: the SSH key file %s is protected by a passphrase, and Farrier never prompts for one. Load it into your SSH agent instead — `ssh-add %s` — and re-run without %s; `%s` lists what the agent is holding",
				path, path, sshKeyFlag, agentListCommand)
		}
		return nil, fmt.Errorf("orchestrate: cannot parse the SSH key file %s: %w. %s wants a private key in OpenSSH or PEM form; a .pub file is the public half and cannot authenticate",
			path, err, sshKeyFlag)
	}
	return ssh.PublicKeys(signer), nil
}

// agentAuth offers whatever the operator's running SSH agent holds.
//
// Both failures here are about the agent not being reachable rather than
// about a key being wrong, so both name the two ways forward: start an
// agent and load a key into it, or stop relying on the agent and name a
// key file.
func agentAuth() (authChoice, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return authChoice{}, fmt.Errorf("orchestrate: no SSH agent to authenticate with: SSH_AUTH_SOCK is not set and no key file was given. %s Start an agent and load a key (`eval $(ssh-agent)`, then `ssh-add ~/.ssh/id_ed25519`), or pass %s with the private key to use",
			authPaths, sshKeyFlag)
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return authChoice{}, fmt.Errorf("orchestrate: cannot reach the SSH agent at %s (from SSH_AUTH_SOCK): %w. The agent that opened this socket is gone — start a new one and load a key (`eval $(ssh-agent)`, then `ssh-add ~/.ssh/id_ed25519`), or pass %s with the private key to use",
			sock, err, sshKeyFlag)
	}
	return authChoice{
		method: ssh.PublicKeysCallback(agent.NewClient(conn).Signers),
		closer: conn,
		source: fmt.Sprintf("the keys your SSH agent at %s offered", sock),
	}, nil
}

// authFailureHint returns the operator-facing explanation to append when
// the host refused every credential Farrier offered. It is empty for every
// other handshake failure — a host key that does not match known_hosts is
// a different problem with different advice, and hostKeyCallback already
// gives it.
//
// Without this the operator gets x/crypto's "attempted methods [none
// publickey], no supported methods remain", which names the SSH protocol's
// method families and nothing an operator can act on: not which credential
// Farrier actually offered, not that a key in ~/.ssh is never offered on
// its own, and not how to see what the agent holds.
func authFailureHint(choice authChoice, err error) string {
	if !isAuthExhausted(err) {
		return ""
	}
	source := choice.source
	if source == "" {
		source = "every credential Farrier offered"
	}
	return fmt.Sprintf(": the host rejected %s. %s Then check that the matching public half is in the authorized_keys of the user in your target",
		source, authPaths)
}

// isAuthExhausted reports whether err is a handshake that ended with every
// credential refused.
//
// x/crypto reports this as a plain fmt.Errorf with no sentinel and no type
// to match on, so its text is the only handle there is. TestConnect...
// AuthRejected pins the coupling: it drives a real handshake against a
// server that refuses the key, so wording that moves out from under this
// fails the test rather than silently dropping the hint.
func isAuthExhausted(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "unable to authenticate") ||
		strings.Contains(msg, "no supported methods remain")
}
