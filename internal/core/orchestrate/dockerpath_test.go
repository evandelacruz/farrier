package orchestrate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withoutDockerPath strips the PATH preamble the transport prepends to every
// command, so a test can assert on the command Farrier meant to run rather
// than its wire form.
func withoutDockerPath(wire string) string {
	return strings.TrimPrefix(wire, dockerPathPreamble)
}

// TestEveryCommandCarriesTheDockerPathFallback pins the placement of the
// fix: it goes on the one session-opening path, so every command from every
// package that reaches a host through this transport gets it. A fix applied
// per caller would leave whichever caller was forgotten broken.
func TestEveryCommandCarriesTheDockerPathFallback(t *testing.T) {
	keyPath, clientSigner := writeTestKeyFile(t)
	server := startRecordingSSHServer(t, clientSigner.PublicKey())
	knownHosts := writeKnownHostsFor(t, server.hostPub, server.addr)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := Connect(ctx, "ssh://tester@"+server.addr, Options{KeyFile: keyPath, KnownHostsFile: knownHosts})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	if err := client.CheckHost(ctx); err != nil {
		t.Fatalf("CheckHost: %v", err)
	}
	if _, err := client.Output(ctx, "docker compose ps"); err != nil {
		t.Fatalf("Output: %v", err)
	}
	if err := client.WriteFile(ctx, "/opt/farrier/compose/docker-compose.yml", []byte("services: {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := client.RunStdin(ctx, "cat > /dev/null", strings.NewReader("x"), nil, nil); err != nil {
		t.Fatalf("RunStdin: %v", err)
	}

	got := server.transcript()
	if len(got) != 4 {
		t.Fatalf("recorded %d exchanges, want 4:\n%s", len(got), formatTranscript(got))
	}
	for i, e := range got {
		if !strings.HasPrefix(e.command, dockerPathPreamble) {
			t.Errorf("exchange %d went out without the docker PATH fallback: %q", i, e.command)
		}
	}
}

// TestDockerPathPreambleResolution runs the preamble through a real /bin/sh
// with a controlled environment. It is the half of the fix that lives in
// shell rather than Go, and the three cases are the three outcomes an
// operator can land in.
func TestDockerPathPreambleResolution(t *testing.T) {
	onPath := t.TempDir()
	home := t.TempDir()
	dockerBin := filepath.Join(home, ".docker", "bin")
	if err := os.MkdirAll(dockerBin, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dockerBin, err)
	}

	writeStub := func(dir, marker string) string {
		t.Helper()
		path := filepath.Join(dir, "docker")
		script := fmt.Sprintf("#!/bin/sh\necho %s\n", marker)
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", path, err)
		}
		return path
	}

	// The command under test is the preamble plus something that runs
	// docker, which is exactly the shape withDockerPath produces.
	run := func(t *testing.T) (string, error) {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-c", withDockerPath("docker"))
		// PATH holds only onPath: the host's real PATH must not decide
		// this test, and neither must a docker installed on the machine
		// running it.
		cmd.Env = []string{"PATH=" + onPath, "HOME=" + home}
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	t.Run("nowhere to be found", func(t *testing.T) {
		out, err := run(t)
		if err == nil {
			t.Fatalf("docker resolved with no docker installed: %q", out)
		}
	})

	t.Run("only where an interactive shell would find it", func(t *testing.T) {
		writeStub(dockerBin, "from-docker-desktop")
		out, err := run(t)
		if err != nil {
			t.Fatalf("docker not found in $HOME/.docker/bin: %v (%s)", err, out)
		}
		if out != "from-docker-desktop" {
			t.Fatalf("ran %q, want the $HOME/.docker/bin docker", out)
		}
	})

	t.Run("already on PATH wins", func(t *testing.T) {
		writeStub(onPath, "from-path")
		out, err := run(t)
		if err != nil {
			t.Fatalf("docker on PATH stopped working: %v (%s)", err, out)
		}
		if out != "from-path" {
			t.Fatalf("ran %q, want the docker already on PATH — the fallback must not shadow it", out)
		}
	})
}

// TestDockerMissingHint covers what an operator reads when docker genuinely
// cannot be found. The hint has to name the reason and the places already
// searched, and it has to stay off failures that are not about docker.
func TestDockerMissingHint(t *testing.T) {
	keyPath, clientSigner := writeTestKeyFile(t)

	dial := func(t *testing.T, handle func(string) fakeResponse) *Client {
		t.Helper()
		addr, hostPub := startFakeSSHServer(t, clientSigner.PublicKey(), handle)
		knownHosts := writeKnownHosts(t, addr, hostPub)
		client, err := Connect(t.Context(), "ssh://tester@"+addr, Options{KeyFile: keyPath, KnownHostsFile: knownHosts})
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		t.Cleanup(func() { client.Close() })
		return client
	}

	notFound := func(string) fakeResponse {
		return fakeResponse{stderr: "zsh:1: command not found: docker\n", exitCode: 127}
	}

	t.Run("docker command exiting 127", func(t *testing.T) {
		client := dial(t, notFound)
		_, err := client.Output(t.Context(), "cd '/opt/farrier' && docker compose up -d")
		if err == nil {
			t.Fatal("Output succeeded against a host with no docker, want error")
		}
		for _, want := range []string{"non-interactive", "$HOME/.docker/bin", "~/.zshenv"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %v does not mention %q", err, want)
			}
		}
	})

	t.Run("CheckHost", func(t *testing.T) {
		client := dial(t, notFound)
		err := client.CheckHost(t.Context())
		if err == nil {
			t.Fatal("CheckHost succeeded against a host with no docker, want error")
		}
		// The host's own stderr stays first — it is the ground truth —
		// and the explanation follows it.
		if !strings.Contains(err.Error(), "command not found: docker") {
			t.Errorf("CheckHost error = %v, want it to carry the host's stderr", err)
		}
		if !strings.Contains(err.Error(), "non-interactive") {
			t.Errorf("CheckHost error = %v, want the explanation of why docker was invisible", err)
		}
	})

	t.Run("some other command exiting 127", func(t *testing.T) {
		client := dial(t, notFound)
		_, err := client.Output(t.Context(), "sqlite3 --version")
		if err == nil {
			t.Fatal("Output succeeded, want error")
		}
		if strings.Contains(err.Error(), "non-interactive") {
			t.Errorf("error %v blames docker for a failure that has nothing to do with it", err)
		}
	})

	t.Run("docker present but failing", func(t *testing.T) {
		client := dial(t, func(string) fakeResponse {
			return fakeResponse{stderr: "Cannot connect to the Docker daemon\n", exitCode: 1}
		})
		err := client.CheckHost(t.Context())
		if err == nil {
			t.Fatal("CheckHost succeeded against an unreachable daemon, want error")
		}
		if strings.Contains(err.Error(), "non-interactive") {
			t.Errorf("error %v suggests a PATH problem for a daemon that is simply down", err)
		}
	})
}
