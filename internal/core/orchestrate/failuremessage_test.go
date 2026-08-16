package orchestrate

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// This file holds the tests for what an operator is told when something
// goes wrong: every failure has to name what failed, why, and what to do
// next. Two failures are the standard the rest are held to — a host that
// refuses every key, and a deploy directory the host will not create —
// because they are the two an operator meets on their first deployment and
// the two whose native diagnosis is furthest from actionable.

// containsAll fails the test naming the first wanted phrase missing from
// got, so an assertion over a paragraph reports which sentence went away
// rather than printing the whole thing back.
func containsAll(t *testing.T, what, got string, want ...string) {
	t.Helper()
	for _, phrase := range want {
		if !strings.Contains(got, phrase) {
			t.Errorf("%s = %q\nmissing: %q", what, got, phrase)
		}
	}
}

// wantsAuthAdvice is what every "the host would not let you in" message
// has to carry: the two credentials Farrier will offer, that a key on disk
// is not one of them by itself, and the command that shows what the agent
// is actually holding.
var wantsAuthAdvice = []string{"-ssh-key", "SSH agent", "not offered just for being on disk", "ssh-add -l"}

// TestConnectKeyFileRejectedExplainsAuthPaths drives a real handshake
// against a server that accepts a different key. What x/crypto reports on
// its own is "attempted methods [none publickey], no supported methods
// remain" — the SSH protocol's method families, which name nothing an
// operator can go change.
//
// This test is also what pins the coupling to that wording: the hint is
// attached by matching x/crypto's text (isAuthExhausted), so wording that
// moves out from under it fails here rather than silently dropping the
// advice.
func TestConnectKeyFileRejectedExplainsAuthPaths(t *testing.T) {
	keyPath, _ := writeTestKeyFile(t)
	_, serverAccepts := newTestKeyPair(t)
	addr, hostPub := startFakeSSHServer(t, serverAccepts.PublicKey(), echoHandler(t))
	knownHosts := writeKnownHosts(t, addr, hostPub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Connect(ctx, fmt.Sprintf("ssh://tester@%s", addr), Options{
		KeyFile:        keyPath,
		KnownHostsFile: knownHosts,
	})
	if err == nil {
		t.Fatal("Connect succeeded with a key the server does not accept, want error")
	}
	containsAll(t, "Connect error", err.Error(),
		append([]string{"the key in " + keyPath, "authorized_keys"}, wantsAuthAdvice...)...)
}

// TestConnectAgentRejectedNamesTheAgent covers the same rejection on the
// other authentication path. The operator has to be able to tell which
// credential was actually offered: an agent quietly holding a key other
// than the one they think they loaded looks identical from the outside to
// a host that forgot their key.
func TestConnectAgentRejectedNamesTheAgent(t *testing.T) {
	sockPath := startTestAgent(t)
	_, serverAccepts := newTestKeyPair(t)
	addr, hostPub := startFakeSSHServer(t, serverAccepts.PublicKey(), echoHandler(t))
	knownHosts := writeKnownHosts(t, addr, hostPub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Connect(ctx, fmt.Sprintf("ssh://tester@%s", addr), Options{KnownHostsFile: knownHosts})
	if err == nil {
		t.Fatal("Connect succeeded with an agent key the server does not accept, want error")
	}
	containsAll(t, "Connect error", err.Error(),
		append([]string{"SSH agent at " + sockPath}, wantsAuthAdvice...)...)
}

// TestConnectUnknownHostKeyKeepsItsOwnDiagnosis holds the line on the
// other side: a host answering with a key that is not in known_hosts is a
// different problem with different advice, and must not be answered with a
// paragraph about ssh-add.
func TestConnectUnknownHostKeyKeepsItsOwnDiagnosis(t *testing.T) {
	keyPath, clientSigner := writeTestKeyFile(t)
	addr, _ := startFakeSSHServer(t, clientSigner.PublicKey(), echoHandler(t))
	knownHosts := writeKnownHosts(t, addr, newTestSigner(t).PublicKey())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Connect(ctx, fmt.Sprintf("ssh://tester@%s", addr), Options{
		KeyFile:        keyPath,
		KnownHostsFile: knownHosts,
	})
	if err == nil {
		t.Fatal("Connect succeeded against an unknown host key, want error")
	}
	if strings.Contains(err.Error(), "ssh-add -l") {
		t.Errorf("host key failure carries authentication advice: %v", err)
	}
}

// TestKeyFileAuthPassphraseProtected covers the key file Farrier can read
// and still cannot use. Farrier never prompts, so "incorrect passphrase"
// would be a dead end; the agent is the mechanism that exists for this and
// the message has to say so.
func TestKeyFileAuthPassphraseProtected(t *testing.T) {
	path := writeEncryptedTestKeyFile(t)

	_, err := authMethod(Options{KeyFile: path})
	if err == nil {
		t.Fatal("authMethod accepted a passphrase-protected key, want error")
	}
	containsAll(t, "authMethod error", err.Error(),
		"passphrase", "never prompts", "ssh-add "+path, "ssh-add -l")
}

// TestKeyFileAuthUnreadable covers the typo: -ssh-key naming a file that
// is not there, or not readable. The way out is either a path that works
// or no flag at all, and both belong in the message.
func TestKeyFileAuthUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent")

	_, err := authMethod(Options{KeyFile: path})
	if err == nil {
		t.Fatal("authMethod accepted a missing key file, want error")
	}
	containsAll(t, "authMethod error", err.Error(), path, "-ssh-key", "ssh-add -l")
}

// TestKeyFileAuthPublicHalf covers pointing -ssh-key at id_ed25519.pub,
// which is a private key file in every way except the one that matters.
func TestKeyFileAuthPublicHalf(t *testing.T) {
	_, signer := newTestKeyPair(t)
	path := filepath.Join(t.TempDir(), "id_test.pub")
	if err := os.WriteFile(path, ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o644); err != nil {
		t.Fatalf("write public key: %v", err)
	}

	_, err := authMethod(Options{KeyFile: path})
	if err == nil {
		t.Fatal("authMethod accepted a public key file, want error")
	}
	containsAll(t, "authMethod error", err.Error(), path, "public half", "-ssh-key")
}

// TestAgentAuthNoSocketSaysHowToGetOne covers the operator with no agent
// running and no key file named — no credential exists at all, and the
// message has to offer both ways to make one.
func TestAgentAuthNoSocketSaysHowToGetOne(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	_, err := authMethod(Options{})
	if err == nil {
		t.Fatal("authMethod succeeded with no agent and no key file, want error")
	}
	containsAll(t, "authMethod error", err.Error(),
		"SSH_AUTH_SOCK", "ssh-agent", "ssh-add", "-ssh-key")
}

// TestAgentAuthStaleSocket covers SSH_AUTH_SOCK naming a socket whose
// agent is gone — a shell that outlived its agent, or a detached session.
// "connection refused" alone tells the operator nothing about what the
// socket is or why Farrier wanted it.
func TestAgentAuthStaleSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "dead.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatalf("write fake socket: %v", err)
	}
	t.Setenv("SSH_AUTH_SOCK", sock)

	_, err := authMethod(Options{})
	if err == nil {
		t.Fatal("authMethod succeeded against a dead agent socket, want error")
	}
	containsAll(t, "authMethod error", err.Error(), sock, "ssh-agent", "-ssh-key")
}

// wantsRemoteDirAdvice is what a refused deploy directory has to say: that
// the default is Farrier's choice and not writable by an ordinary user,
// and that a directory the operator already owns can be given instead.
var wantsRemoteDirAdvice = []string{DefaultRemoteDir, "belongs to root", "-remote-dir"}

// TestWriteFileRefusedExplainsRemoteDir is the first wall an operator
// deploying to their own machine walks into: `up` ships a rendered config
// file into /opt/farrier and the host will not create it. The failure
// travels through WriteFile, so that is where this checks it — end to end
// over a real session, not against the hint function.
func TestWriteFileRefusedExplainsRemoteDir(t *testing.T) {
	client, _ := connectToRefusingHost(t)

	err := client.WriteFile(context.Background(), DefaultRemoteDir+"/forge/app.ini", []byte("x"), 0o600)
	if err == nil {
		t.Fatal("WriteFile succeeded against a host that refuses the path, want error")
	}
	containsAll(t, "WriteFile error", err.Error(),
		append([]string{"Permission denied"}, wantsRemoteDirAdvice...)...)
}

// TestOutputRefusedExplainsRemoteDir covers the same refusal on the other
// path a caller creates a directory by (deploy's state directories go
// through Output), so the advice does not depend on which one a given step
// happens to use.
func TestOutputRefusedExplainsRemoteDir(t *testing.T) {
	client, _ := connectToRefusingHost(t)

	_, err := client.Output(context.Background(), "mkdir -p '"+DefaultRemoteDir+"/state/git'")
	if err == nil {
		t.Fatal("Output succeeded against a host that refuses the path, want error")
	}
	containsAll(t, "Output error", err.Error(),
		append([]string{"Permission denied"}, wantsRemoteDirAdvice...)...)
}

// TestRunStdinRefusedExplainsRemoteDir covers restore's path, which
// streams a tar into a directory it creates and supplies its own stderr
// writer. It is the reason run taps stderr rather than reading a buffer
// one of its callers happens to keep: a caller taking stderr for itself
// must not cost the operator the diagnosis.
func TestRunStdinRefusedExplainsRemoteDir(t *testing.T) {
	client, _ := connectToRefusingHost(t)

	var stderr bytes.Buffer
	command := "mkdir -p '" + DefaultRemoteDir + "/state/git' && tar -C '" + DefaultRemoteDir + "/state/git' -xf -"
	err := client.RunStdin(context.Background(), command, strings.NewReader(""), nil, &stderr)
	if err == nil {
		t.Fatal("RunStdin succeeded against a host that refuses the path, want error")
	}
	containsAll(t, "RunStdin error", err.Error(), wantsRemoteDirAdvice...)
	if !strings.Contains(stderr.String(), "Permission denied") {
		t.Errorf("caller's stderr = %q, want the host's own message", stderr.String())
	}
}

// TestRemoteDirHintStaysOffUnrelatedFailures holds the hint to the failure
// it explains. A refusal that -remote-dir would do nothing about — the
// Docker socket an operator outside the docker group is denied on every
// host — must keep its own diagnosis, and so must a path that failed for a
// reason other than permission.
func TestRemoteDirHintStaysOffUnrelatedFailures(t *testing.T) {
	cases := []struct {
		name    string
		command string
		stderr  string
	}{
		{
			name:    "docker socket denied",
			command: "docker version --format '{{.Server.Version}}'",
			stderr:  "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock",
		},
		{
			name:    "disk full",
			command: "mkdir -p '/opt/farrier/state/git'",
			stderr:  "mkdir: cannot create directory '/opt/farrier': No space left on device",
		},
		{
			name:    "nothing on stderr",
			command: "mkdir -p '/opt/farrier'",
			stderr:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if hint := remoteDirHint(tc.command, tc.stderr); hint != "" {
				t.Errorf("remoteDirHint = %q, want none", hint)
			}
		})
	}
}

// TestRemoteDirHintCoversEveryRefusal covers the refusals a host actually
// answers with. A read-only filesystem and an operation that is not
// permitted are the same problem as a denied permission from where the
// operator sits, and get the same way out.
func TestRemoteDirHintCoversEveryRefusal(t *testing.T) {
	for _, stderr := range []string{
		"mkdir: cannot create directory '/opt/farrier': Permission denied",
		"mkdir: cannot create directory '/opt/farrier': Read-only file system",
		"touch: /opt/farrier/state/gitea/conf/app.ini: Operation not permitted",
	} {
		if hint := remoteDirHint("mkdir -p '/opt/farrier' && touch '/opt/farrier/x'", stderr); hint == "" {
			t.Errorf("remoteDirHint(%q) = empty, want the deploy-directory advice", stderr)
		}
	}
}

// TestStderrTapBounded keeps a diagnosis from turning into a memory
// problem: a command that fails while writing megabytes to stderr is still
// only diagnosed from its first lines, and the caller's own writer still
// receives every byte.
func TestStderrTapBounded(t *testing.T) {
	var tap stderrTap
	chunk := bytes.Repeat([]byte("x"), stderrTapLimit)

	for range 3 {
		n, err := tap.Write(chunk)
		if err != nil || n != len(chunk) {
			t.Fatalf("Write = %d, %v, want %d, nil", n, err, len(chunk))
		}
	}
	if got := len(tap.String()); got != stderrTapLimit {
		t.Fatalf("tap length = %d, want %d", got, stderrTapLimit)
	}
}

// connectToRefusingHost returns a client connected to a fake host that
// refuses every command with the message a POSIX host gives for a
// directory an ordinary user cannot create.
func connectToRefusingHost(t *testing.T) (*Client, string) {
	t.Helper()
	keyPath, clientSigner := writeTestKeyFile(t)
	addr, hostPub := startFakeSSHServer(t, clientSigner.PublicKey(), func(string) fakeResponse {
		return fakeResponse{
			stderr:   "mkdir: cannot create directory '" + DefaultRemoteDir + "': Permission denied\n",
			exitCode: 1,
		}
	})
	knownHosts := writeKnownHosts(t, addr, hostPub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := Connect(ctx, fmt.Sprintf("ssh://tester@%s", addr), Options{
		KeyFile:        keyPath,
		KnownHostsFile: knownHosts,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client, addr
}

// startTestAgent runs an in-memory SSH agent holding one key on a unix
// socket, points SSH_AUTH_SOCK at it, and returns the socket path.
func startTestAgent(t *testing.T) string {
	t.Helper()
	priv, _ := newTestKeyPair(t)

	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatalf("add key to agent: %v", err)
	}
	sockPath := filepath.Join(t.TempDir(), "agent.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen agent socket: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go agent.ServeAgent(keyring, conn)
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", sockPath)
	return sockPath
}

// writeEncryptedTestKeyFile writes a passphrase-protected ed25519 key to a
// file under t.TempDir() and returns its path.
func writeEncryptedTestKeyFile(t *testing.T) string {
	t.Helper()
	priv, _ := newTestKeyPair(t)

	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("hunter2"))
	if err != nil {
		t.Fatalf("marshal encrypted key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id_encrypted")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}
