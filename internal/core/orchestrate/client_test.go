package orchestrate

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// writeKnownHosts writes a known_hosts file recording hostPub for addr and
// returns its path.
func writeKnownHosts(t *testing.T, addr string, hostPub ssh.PublicKey) string {
	t.Helper()
	line := knownhosts.Line([]string{knownhosts.Normalize(addr)}, hostPub)
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	return path
}

func echoHandler(t *testing.T) func(string) fakeResponse {
	return func(command string) fakeResponse {
		if strings.HasPrefix(command, "docker version") {
			return fakeResponse{stdout: "26.1.0\n", exitCode: 0}
		}
		return fakeResponse{stdout: command + "\n", exitCode: 0}
	}
}

func TestConnectRunKeyFile(t *testing.T) {
	keyPath, clientSigner := writeTestKeyFile(t)
	addr, hostPub := startFakeSSHServer(t, clientSigner.PublicKey(), echoHandler(t))
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
	defer client.Close()

	var stdout bytes.Buffer
	if err := client.Run(ctx, "echo hello", &stdout, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := stdout.String(); got != "echo hello\n" {
		t.Fatalf("Run stdout = %q, want %q", got, "echo hello\n")
	}
}

func TestConnectUnknownHostKeyFails(t *testing.T) {
	keyPath, clientSigner := writeTestKeyFile(t)
	addr, _ := startFakeSSHServer(t, clientSigner.PublicKey(), echoHandler(t))

	// A known_hosts file that records some other host's key: addr is
	// present but the key won't match, so the connection must fail
	// closed rather than trust the server's real key.
	otherSigner := newTestSigner(t)
	knownHosts := writeKnownHosts(t, addr, otherSigner.PublicKey())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Connect(ctx, fmt.Sprintf("ssh://tester@%s", addr), Options{
		KeyFile:        keyPath,
		KnownHostsFile: knownHosts,
	})
	if err == nil {
		t.Fatal("Connect succeeded against a host key mismatch, want error")
	}
}

func TestConnectMissingKnownHostsFails(t *testing.T) {
	keyPath, clientSigner := writeTestKeyFile(t)
	addr, _ := startFakeSSHServer(t, clientSigner.PublicKey(), echoHandler(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Connect(ctx, fmt.Sprintf("ssh://tester@%s", addr), Options{
		KeyFile:        keyPath,
		KnownHostsFile: filepath.Join(t.TempDir(), "does-not-exist"),
	})
	if err == nil {
		t.Fatal("Connect succeeded with no known_hosts file, want error")
	}
}

func TestConnectAgentAuth(t *testing.T) {
	clientPriv, clientSigner := newTestKeyPair(t)

	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: clientPriv}); err != nil {
		t.Fatalf("add key to agent: %v", err)
	}
	sockPath := filepath.Join(t.TempDir(), "agent.sock")
	agentListener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen agent socket: %v", err)
	}
	t.Cleanup(func() { agentListener.Close() })
	go func() {
		for {
			conn, err := agentListener.Accept()
			if err != nil {
				return
			}
			go agent.ServeAgent(keyring, conn)
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", sockPath)

	addr, hostPub := startFakeSSHServer(t, clientSigner.PublicKey(), echoHandler(t))
	knownHosts := writeKnownHosts(t, addr, hostPub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := Connect(ctx, fmt.Sprintf("ssh://tester@%s", addr), Options{KnownHostsFile: knownHosts})
	if err != nil {
		t.Fatalf("Connect via agent: %v", err)
	}
	defer client.Close()
}

func TestConnectNoAuthConfiguredFails(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := Connect(ctx, "ssh://tester@127.0.0.1:1", Options{})
	if err == nil {
		t.Fatal("Connect succeeded with no agent and no key file, want error")
	}
	if !strings.Contains(err.Error(), "SSH_AUTH_SOCK") {
		t.Fatalf("Connect error = %v, want mention of SSH_AUTH_SOCK", err)
	}
}

func TestCheckHost(t *testing.T) {
	keyPath, clientSigner := writeTestKeyFile(t)

	t.Run("docker present", func(t *testing.T) {
		addr, hostPub := startFakeSSHServer(t, clientSigner.PublicKey(), echoHandler(t))
		knownHosts := writeKnownHosts(t, addr, hostPub)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		client, err := Connect(ctx, fmt.Sprintf("ssh://tester@%s", addr), Options{KeyFile: keyPath, KnownHostsFile: knownHosts})
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer client.Close()

		if err := client.CheckHost(ctx); err != nil {
			t.Fatalf("CheckHost: %v", err)
		}
	})

	t.Run("docker missing", func(t *testing.T) {
		addr, hostPub := startFakeSSHServer(t, clientSigner.PublicKey(), func(command string) fakeResponse {
			return fakeResponse{stderr: "bash: docker: command not found\n", exitCode: 127}
		})
		knownHosts := writeKnownHosts(t, addr, hostPub)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		client, err := Connect(ctx, fmt.Sprintf("ssh://tester@%s", addr), Options{KeyFile: keyPath, KnownHostsFile: knownHosts})
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer client.Close()

		err = client.CheckHost(ctx)
		if err == nil {
			t.Fatal("CheckHost succeeded against a host with no Docker, want error")
		}
		if !strings.Contains(err.Error(), "docker: command not found") {
			t.Fatalf("CheckHost error = %v, want it to name the Docker failure", err)
		}
	})
}
