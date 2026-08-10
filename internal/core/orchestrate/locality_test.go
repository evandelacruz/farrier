package orchestrate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// ORCH-003: a target of ssh://user@localhost runs the identical path as any
// remote host — no local mode, no branch on locality, nothing skipped.
//
// The requirement is an invariant rather than a feature, so the tests here
// are what enforce it. They come in two halves.
//
// The first half is behavioural: a real SSH connection, addressed as
// localhost, is driven through a deploy-shaped sequence and every byte it
// puts on the wire is compared against the same sequence run by a client
// that believes it is talking to a VPS. Identical transcripts mean the
// deployment path does not care where the host is.
//
// The second half is structural (locality_guard_test.go): the behavioural
// test can only prove that today's code has no locality branch, and a
// future one could grow a branch it never exercises. The guard reads the
// tree instead of running it, and fails when product code starts asking
// whether a host is local.

// localityHostKeys are the spellings a loopback host arrives as, alongside
// the remote name they are checked against. Every one of them is an
// ordinary host: nothing here is a list of names to treat specially, it is
// the list of names that must not be treated specially.
var localityHostKeys = []string{"localhost", "127.0.0.1", "::1", "a-vps.example.com"}

// TestLoopbackTargetsAreOrdinaryTargets pins the first place a locality
// branch could appear: parsing. A loopback spelling must produce the same
// shape of Target as a remote name, default its port the same way, dial the
// same form of address, and round-trip through String identically.
func TestLoopbackTargetsAreOrdinaryTargets(t *testing.T) {
	for _, host := range localityHostKeys {
		t.Run(host, func(t *testing.T) {
			// ::1 has to be bracketed in a URL like any other IPv6
			// literal; that is URL syntax, not a loopback rule.
			literal := host
			if strings.Contains(host, ":") {
				literal = "[" + host + "]"
			}

			got, err := ParseTarget("ssh://ops@" + literal)
			if err != nil {
				t.Fatalf("ParseTarget(ssh://ops@%s): %v", literal, err)
			}
			want := Target{User: "ops", Host: host, Port: DefaultPort}
			if got != want {
				t.Fatalf("ParseTarget(ssh://ops@%s) = %+v, want %+v", literal, got, want)
			}
			if addr, wantAddr := got.addr(), net.JoinHostPort(host, DefaultPort); addr != wantAddr {
				t.Errorf("addr() = %q, want %q", addr, wantAddr)
			}

			// An explicit port must survive for a loopback host exactly
			// as it does for a remote one.
			withPort, err := ParseTarget("ssh://ops@" + net.JoinHostPort(literal, "2200"))
			if err != nil {
				t.Fatalf("ParseTarget with explicit port: %v", err)
			}
			if withPort.Port != "2200" {
				t.Errorf("explicit port = %q, want 2200", withPort.Port)
			}

			// String is the form written into recovery notes and event
			// details, and it must not describe a loopback host in some
			// other way.
			reparsed, err := ParseTarget(got.String())
			if err != nil {
				t.Fatalf("ParseTarget(%q): %v", got.String(), err)
			}
			if reparsed != got {
				t.Errorf("String round-trip = %+v, want %+v", reparsed, got)
			}

			// The rejections are part of "identical path" too: a
			// loopback target with no user is as invalid as a remote one,
			// rather than being filled in from the local environment.
			if _, err := ParseTarget("ssh://" + literal); err == nil {
				t.Errorf("ParseTarget(ssh://%s): want error for missing user, got nil", literal)
			}
			if _, err := ParseTarget("https://ops@" + literal); err == nil {
				t.Errorf("ParseTarget(https://ops@%s): want error for wrong scheme, got nil", literal)
			}
		})
	}
}

// TestLocalhostRunsTheIdenticalPath is ORCH-003's behavioural half.
//
// One in-process SSH server records every command it is asked to exec and
// everything streamed to that command's stdin. Three clients drive the same
// deploy-shaped sequence against it: one reached as ssh://tester@localhost,
// one as ssh://tester@127.0.0.1, and one holding a Target that names a
// remote VPS. All three must produce byte-identical transcripts — same
// commands, same order, same content, nothing added and nothing skipped.
func TestLocalhostRunsTheIdenticalPath(t *testing.T) {
	keyPath, clientSigner := writeTestKeyFile(t)
	server := startRecordingSSHServer(t, clientSigner.PublicKey())

	_, port, err := net.SplitHostPort(server.addr)
	if err != nil {
		t.Fatalf("split server address %q: %v", server.addr, err)
	}

	// The server listens on 127.0.0.1, so both loopback spellings reach
	// it. known_hosts records the host key under each, exactly as the
	// operator's own file would after connecting once with ssh.
	knownHosts := writeKnownHostsFor(t, server.hostPub,
		net.JoinHostPort("localhost", port),
		net.JoinHostPort("127.0.0.1", port),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	dial := func(host string) *Client {
		t.Helper()
		client, err := Connect(ctx, "ssh://tester@"+net.JoinHostPort(host, port), Options{
			KeyFile:        keyPath,
			KnownHostsFile: knownHosts,
		})
		if err != nil {
			t.Fatalf("Connect to %s: %v", host, err)
		}
		t.Cleanup(func() { client.Close() })
		return client
	}

	byName := dial("localhost")
	byIP := dial("127.0.0.1")

	// The third client shares byName's connection but carries a remote
	// Target: same host, same session, different belief about where that
	// host is. Anything it did differently would be a locality branch
	// downstream of Connect. It deliberately does not get a Cleanup that
	// closes the shared connection twice.
	asRemote := &Client{
		target: Target{User: "tester", Host: "a-vps.example.com", Port: DefaultPort},
		conn:   byName.conn,
	}

	cases := []struct {
		name   string
		client *Client
	}{
		{name: "localhost", client: byName},
		{name: "127.0.0.1", client: byIP},
		{name: "a-vps.example.com", client: asRemote},
	}

	var reference []exchange
	for _, tc := range cases {
		server.reset()
		if err := driveDeployShapedSequence(ctx, tc.client); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := server.transcript()
		if len(got) == 0 {
			t.Fatalf("%s: server recorded nothing", tc.name)
		}

		if reference == nil {
			// Equality is only worth having if the transcript it
			// compares is the whole deployment. Two clients that both
			// skipped everything would agree perfectly.
			assertDeployShape(t, got)
			reference = got
			continue
		}
		if len(got) != len(reference) {
			t.Fatalf("%s issued %d exchanges, localhost issued %d:\n%s\nvs\n%s",
				tc.name, len(got), len(reference), formatTranscript(got), formatTranscript(reference))
		}
		for i := range got {
			if got[i] != reference[i] {
				t.Fatalf("%s diverged from the localhost path at exchange %d:\n got: %+v\nwant: %+v",
					tc.name, i, got[i], reference[i])
			}
		}
	}
}

// driveDeployShapedSequence runs the operations a real deployment puts a
// host through — the Docker check, a full Compose converge, a config file
// shipped alongside it, and a status read afterwards. It covers all three
// Transport methods plus CheckHost, which is every way this package touches
// a host.
func driveDeployShapedSequence(ctx context.Context, client *Client) error {
	if err := client.CheckHost(ctx); err != nil {
		return fmt.Errorf("CheckHost: %w", err)
	}
	if err := Converge(ctx, client, "/opt/farrier", testBundle()); err != nil {
		return fmt.Errorf("Converge: %w", err)
	}
	if err := client.WriteFile(ctx, "/opt/farrier/forge/app.ini", []byte("[server]\nDOMAIN = forge.example.com\n"), 0o600); err != nil {
		return fmt.Errorf("WriteFile: %w", err)
	}
	if _, err := client.Output(ctx, "docker ps --format '{{.Names}}'"); err != nil {
		return fmt.Errorf("Output: %w", err)
	}
	return nil
}

// assertDeployShape checks the localhost leg actually put a host through
// every step of driveDeployShapedSequence — the Docker check, the staged
// Compose directory, both file writes with their content arriving on stdin,
// the swap into place, `docker compose up`, and the status read. This is
// the "nothing skipped" half of ORCH-003: without it, the equality check
// above would be satisfied by three clients that each did nothing.
func assertDeployShape(t *testing.T, got []exchange) {
	t.Helper()

	steps := []struct {
		what  string
		match func(exchange) bool
	}{
		{"docker version check", func(e exchange) bool {
			return strings.HasPrefix(e.command, "docker version")
		}},
		{"compose staging directory", func(e exchange) bool {
			return strings.Contains(e.command, "mkdir -p") && strings.Contains(e.command, "compose.tmp")
		}},
		{"compose file shipped with its content", func(e exchange) bool {
			return strings.Contains(e.command, "docker-compose.yml") && e.stdin == "services: {}\n"
		}},
		{"staging directory swapped into place", func(e exchange) bool {
			return strings.Contains(e.command, "mv") && strings.Contains(e.command, "compose.tmp")
		}},
		{"docker compose up", func(e exchange) bool {
			return strings.Contains(e.command, "docker compose up -d")
		}},
		{"app.ini shipped with its content", func(e exchange) bool {
			return strings.Contains(e.command, "app.ini") && strings.Contains(e.stdin, "DOMAIN = forge.example.com")
		}},
		{"status read", func(e exchange) bool {
			return strings.HasPrefix(e.command, "docker ps")
		}},
	}

	next := 0
	for _, e := range got {
		if next < len(steps) && steps[next].match(e) {
			next++
		}
	}
	if next != len(steps) {
		t.Fatalf("localhost transcript is missing %q (and everything after it):\n%s",
			steps[next].what, formatTranscript(got))
	}
}

// exchange is one command a client asked the host to run, together with
// everything it streamed to that command's stdin. Comparable, so transcripts
// can be checked with ==.
type exchange struct {
	command string
	stdin   string
}

func formatTranscript(t []exchange) string {
	var b strings.Builder
	for i, e := range t {
		fmt.Fprintf(&b, "  %d. %s", i, e.command)
		if e.stdin != "" {
			fmt.Fprintf(&b, " <<< %q", e.stdin)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// recordingSSHServer is an SSH server that records the full exchange for
// every command rather than only its text. It exists alongside
// startFakeSSHServer (canned responses, stdin ignored) and
// startShellSSHServer (real /bin/sh, nothing recorded) because ORCH-003
// needs the one thing neither provides: the exact bytes a client put on the
// wire, so two runs can be compared for equality.
type recordingSSHServer struct {
	addr    string
	hostPub ssh.PublicKey

	mu        sync.Mutex
	exchanges []exchange
}

func (s *recordingSSHServer) record(e exchange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exchanges = append(s.exchanges, e)
}

func (s *recordingSSHServer) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exchanges = nil
}

func (s *recordingSSHServer) transcript() []exchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]exchange(nil), s.exchanges...)
}

func startRecordingSSHServer(t *testing.T, clientPub ssh.PublicKey) *recordingSSHServer {
	t.Helper()

	hostSigner := newTestSigner(t)
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !bytes.Equal(key.Marshal(), clientPub.Marshal()) {
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

	server := &recordingSSHServer{addr: listener.Addr().String(), hostPub: hostSigner.PublicKey()}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.serveConn(conn, config)
		}
	}()
	return server
}

func (s *recordingSSHServer) serveConn(netConn net.Conn, config *ssh.ServerConfig) {
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
		go s.serveSession(channel, requests)
	}
}

func (s *recordingSSHServer) serveSession(channel ssh.Channel, requests <-chan *ssh.Request) {
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

		// Read to EOF before recording: the client closes the write side
		// once it has streamed the whole payload, so this is the complete
		// stdin for the command.
		stdin, _ := io.ReadAll(channel)
		s.record(exchange{command: msg.Command, stdin: string(stdin)})

		if strings.HasPrefix(msg.Command, "docker version") {
			io.WriteString(channel, "26.1.0\n")
		}
		channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
		return
	}
}

// writeKnownHostsFor records hostPub against every given host:port, the way
// an operator's known_hosts holds one entry per address they have reached a
// host at.
func writeKnownHostsFor(t *testing.T, hostPub ssh.PublicKey, addrs ...string) string {
	t.Helper()
	var lines []string
	for _, addr := range addrs {
		lines = append(lines, knownhosts.Line([]string{knownhosts.Normalize(addr)}, hostPub))
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	return path
}
