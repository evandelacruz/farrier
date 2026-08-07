package orchestrate

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"

	"golang.org/x/crypto/ssh"
)

var errUnauthorizedTestKey = errors.New("orchestrate test: unrecognized client key")

// fakeResponse is what a fake server sends back for one exec'd command.
type fakeResponse struct {
	stdout   string
	stderr   string
	exitCode uint32
}

// startFakeSSHServer starts a minimal SSH server on 127.0.0.1: it accepts
// only clientPub for authentication and answers every "exec" request via
// handle. It exists so Connect, Run, and CheckHost can be exercised against
// a real (if tiny) SSH handshake without an actual host.
func startFakeSSHServer(t *testing.T, clientPub ssh.PublicKey, handle func(command string) fakeResponse) (addr string, hostPub ssh.PublicKey) {
	t.Helper()

	hostSigner := newTestSigner(t)

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if clientPub == nil || !bytes.Equal(key.Marshal(), clientPub.Marshal()) {
				return nil, errUnauthorizedTestKey
			}
			return nil, nil
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
			netConn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveFakeConn(netConn, config, handle)
		}
	}()

	return listener.Addr().String(), hostSigner.PublicKey()
}

func serveFakeConn(netConn net.Conn, config *ssh.ServerConfig, handle func(string) fakeResponse) {
	sconn, chans, reqs, err := ssh.NewServerConn(netConn, config)
	if err != nil {
		netConn.Close()
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
			return
		}
		go serveFakeSession(channel, requests, handle)
	}
}

func serveFakeSession(channel ssh.Channel, requests <-chan *ssh.Request, handle func(string) fakeResponse) {
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

		resp := handle(msg.Command)
		io.WriteString(channel, resp.stdout)
		io.WriteString(channel.Stderr(), resp.stderr)
		channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{resp.exitCode}))
		return
	}
}
