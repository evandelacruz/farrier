package orchestrate

import (
	"errors"
	"net"
	"os/exec"
	"testing"

	"golang.org/x/crypto/ssh"
)

// startShellSSHServer starts an SSH server on 127.0.0.1 that runs every
// exec'd command through a real /bin/sh, with the session's stdin wired to
// the command's stdin.
//
// It exists alongside startFakeSSHServer because the two answer different
// questions. The fake server replays a canned fakeResponse per command,
// which is all Run and CheckHost need. WriteFile cannot be tested that way:
// its contract is about what ends up on the filesystem — parent directories
// created, permissions applied, the target replaced only once the content
// has fully arrived — and asserting that requires the command to actually
// execute and consume stdin.
func startShellSSHServer(t *testing.T, clientPub ssh.PublicKey) (addr string, hostPub ssh.PublicKey) {
	t.Helper()

	hostSigner := newTestSigner(t)
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(clientPub.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errUnauthorizedTestKey
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
			go serveShellConn(conn, config)
		}
	}()

	return listener.Addr().String(), hostSigner.PublicKey()
}

func serveShellConn(netConn net.Conn, config *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(netConn, config)
	if err != nil {
		netConn.Close()
		return
	}
	defer conn.Close()
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
		go serveShellSession(channel, requests)
	}
}

func serveShellSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for req := range requests {
		if req.Type != "exec" {
			if req.WantReply {
				req.Reply(false, nil)
			}
			continue
		}

		var msg struct{ Command string }
		ssh.Unmarshal(req.Payload, &msg)
		req.Reply(true, nil)

		cmd := exec.Command("sh", "-c", msg.Command)
		cmd.Stdin = channel
		cmd.Stdout = channel
		cmd.Stderr = channel.Stderr()
		runErr := cmd.Run()

		var status uint32
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			status = uint32(exitErr.ExitCode())
		} else if runErr != nil {
			status = 1
		}
		channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}

// dialShellServer connects a Client to a shell-backed test server.
func dialShellServer(t *testing.T) *Client {
	t.Helper()
	keyPath, clientSigner := writeTestKeyFile(t)
	addr, hostPub := startShellSSHServer(t, clientSigner.PublicKey())
	knownHosts := writeKnownHosts(t, addr, hostPub)

	client, err := Connect(t.Context(), "ssh://tester@"+addr, Options{
		KeyFile:        keyPath,
		KnownHostsFile: knownHosts,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}
