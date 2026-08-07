package orchestrate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// testSSHServer is a minimal in-process SSH server that authenticates one
// public key and runs "exec" requests through a real shell, so SSHTransport
// can be tested end to end (dial, auth, host-key check, run, write file)
// without a real host.
type testSSHServer struct {
	addr string
}

func startTestSSHServer(t *testing.T, authorizedKey ssh.PublicKey) (*testSSHServer, ssh.PublicKey) {
	t.Helper()

	hostPub, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromSigner(hostPriv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), authorizedKey.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unauthorized key")
		},
	}
	config.AddHostKey(hostSigner)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveTestSSHConn(conn, config)
		}
	}()

	hostPubKey, err := ssh.NewPublicKey(hostPub)
	if err != nil {
		t.Fatalf("host public key: %v", err)
	}
	return &testSSHServer{addr: listener.Addr().String()}, hostPubKey
}

func serveTestSSHConn(conn net.Conn, config *ssh.ServerConfig) {
	sconn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		conn.Close()
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go serveTestSSHSession(channel, requests)
	}
}

func serveTestSSHSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for req := range requests {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		ssh.Unmarshal(req.Payload, &payload)
		req.Reply(true, nil)

		cmd := exec.Command("sh", "-c", payload.Command)
		cmd.Stdin = channel
		cmd.Stdout = channel
		cmd.Stderr = channel.Stderr()
		runErr := cmd.Run()

		exitCode := 0
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if runErr != nil {
			exitCode = 1
		}
		channel.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{uint32(exitCode)}))
		return
	}
}

// testClientIdentity generates an ed25519 keypair, writes the private key
// to an unencrypted PEM file under t.TempDir, and returns the file path
// alongside the public key so the test server can authorize it.
func testClientIdentity(t *testing.T) (keyPath string, pub ssh.PublicKey) {
	t.Helper()
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		t.Fatalf("client public key: %v", err)
	}
	return path, sshPub
}

func testKnownHosts(t *testing.T, addr string, hostKey ssh.PublicKey) string {
	t.Helper()
	line := knownhosts.Line([]string{knownhosts.Normalize(addr)}, hostKey)
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	return path
}

func dialTestServer(t *testing.T) *SSHTransport {
	t.Helper()
	keyPath, pub := testClientIdentity(t)
	server, hostPub := startTestSSHServer(t, pub)
	knownHosts := testKnownHosts(t, server.addr, hostPub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transport, err := DialSSH(ctx, SSHConfig{
		Target:         "ssh://tester@" + server.addr,
		KeyPath:        keyPath,
		KnownHostsPath: knownHosts,
		Timeout:        5 * time.Second,
	})
	if err != nil {
		t.Fatalf("DialSSH: %v", err)
	}
	t.Cleanup(func() { transport.Close() })
	return transport
}

func TestSSHTransportRun(t *testing.T) {
	transport := dialTestServer(t)

	out, err := transport.Run(context.Background(), "echo -n hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(out) != "hello" {
		t.Errorf("Run output = %q, want %q", out, "hello")
	}
}

func TestSSHTransportRunFailureIncludesStderr(t *testing.T) {
	transport := dialTestServer(t)

	_, err := transport.Run(context.Background(), "echo boom >&2 && exit 3")
	if err == nil {
		t.Fatal("Run: want error for nonzero exit, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("boom")) {
		t.Errorf("Run error = %q, want it to include stderr", err)
	}
}

func TestSSHTransportWriteFile(t *testing.T) {
	transport := dialTestServer(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "sub", "file.txt")

	if err := transport.WriteFile(context.Background(), target, []byte("payload"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("content = %q, want %q", got, "payload")
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode().Perm())
	}

	// No leftover staging file.
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("staging file left behind: err = %v", err)
	}
}

func TestSSHTransportWriteFileNoPartialOnFailure(t *testing.T) {
	// WriteFile targets a directory it cannot create (a path that
	// traverses a file, not a directory), so the write fails and the
	// final file must never appear.
	transport := dialTestServer(t)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	target := filepath.Join(blocker, "file.txt")

	if err := transport.WriteFile(context.Background(), target, []byte("payload"), 0o644); err == nil {
		t.Fatal("WriteFile: want error, got nil")
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("target should not exist, but os.Stat found it")
	}
}

func TestDialSSHRejectsBadTarget(t *testing.T) {
	_, err := DialSSH(context.Background(), SSHConfig{Target: "not-a-target"})
	if err == nil {
		t.Fatal("DialSSH: want error for malformed target, got nil")
	}
}

func TestDialSSHRejectsUnknownHostKey(t *testing.T) {
	keyPath, pub := testClientIdentity(t)
	server, _ := startTestSSHServer(t, pub)

	// A known_hosts file with no entry for this server must fail the
	// handshake instead of silently trusting the host.
	emptyKnownHosts := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(emptyKnownHosts, nil, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := DialSSH(ctx, SSHConfig{
		Target:         "ssh://tester@" + server.addr,
		KeyPath:        keyPath,
		KnownHostsPath: emptyKnownHosts,
		Timeout:        5 * time.Second,
	})
	if err == nil {
		t.Fatal("DialSSH: want error for unknown host key, got nil")
	}
}
