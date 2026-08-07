package state

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalGitExporterRemotes(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "acme", "widgets.git"))
	mustMkdirAll(t, filepath.Join(root, "acme", "gadgets.git"))
	mustMkdirAll(t, filepath.Join(root, "beta", "tools.git"))
	// Non-repo entries under an owner directory must be ignored.
	mustMkdirAll(t, filepath.Join(root, "acme", "not-a-repo"))
	if err := os.WriteFile(filepath.Join(root, "acme", "README"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	// A non-directory entry directly under root must be ignored too.
	if err := os.WriteFile(filepath.Join(root, "stray-file"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exporter := &LocalGitExporter{Root: root}
	remotes, err := exporter.Remotes(context.Background())
	if err != nil {
		t.Fatalf("Remotes: %v", err)
	}

	want := []Remote{
		{Name: "acme/gadgets", URL: filepath.Join(root, "acme", "gadgets.git")},
		{Name: "acme/widgets", URL: filepath.Join(root, "acme", "widgets.git")},
		{Name: "beta/tools", URL: filepath.Join(root, "beta", "tools.git")},
	}
	if !remotesEqual(remotes, want) {
		t.Fatalf("Remotes = %+v, want %+v", remotes, want)
	}
}

func TestLocalGitExporterMissingRoot(t *testing.T) {
	exporter := &LocalGitExporter{Root: filepath.Join(t.TempDir(), "does-not-exist")}
	remotes, err := exporter.Remotes(context.Background())
	if err != nil {
		t.Fatalf("Remotes: %v", err)
	}
	if len(remotes) != 0 {
		t.Fatalf("Remotes = %+v, want empty", remotes)
	}
}

func TestLocalGitExporterRequiresRoot(t *testing.T) {
	exporter := &LocalGitExporter{}
	if _, err := exporter.Remotes(context.Background()); err == nil {
		t.Fatal("Remotes succeeded with no root, want error")
	}
}

// fakeRunner records the command it was asked to run and answers with a
// canned stdout/stderr/error, standing in for *orchestrate.Client in tests.
type fakeRunner struct {
	command string
	stdout  string
	stderr  string
	err     error
}

func (f *fakeRunner) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	f.command = command
	if stdout != nil {
		io.WriteString(stdout, f.stdout)
	}
	if stderr != nil {
		io.WriteString(stderr, f.stderr)
	}
	return f.err
}

func TestSSHGitExporterRemotes(t *testing.T) {
	runner := &fakeRunner{
		stdout: "/data/git/acme/widgets.git\n/data/git/beta/tools.git\n",
	}
	exporter := &SSHGitExporter{
		Runner: runner,
		User:   "farrier",
		Host:   "forge.example.com",
		Port:   "2222",
		Root:   "/data/git",
	}

	remotes, err := exporter.Remotes(context.Background())
	if err != nil {
		t.Fatalf("Remotes: %v", err)
	}

	want := []Remote{
		{Name: "acme/widgets", URL: "ssh://farrier@forge.example.com:2222/data/git/acme/widgets.git"},
		{Name: "beta/tools", URL: "ssh://farrier@forge.example.com:2222/data/git/beta/tools.git"},
	}
	if !remotesEqual(remotes, want) {
		t.Fatalf("Remotes = %+v, want %+v", remotes, want)
	}

	if !strings.Contains(runner.command, "find '/data/git'") {
		t.Fatalf("command = %q, want it to find under the quoted root", runner.command)
	}
}

func TestSSHGitExporterDefaultsPort(t *testing.T) {
	runner := &fakeRunner{stdout: "/data/git/acme/widgets.git\n"}
	exporter := &SSHGitExporter{Runner: runner, User: "farrier", Host: "forge.example.com", Root: "/data/git"}

	remotes, err := exporter.Remotes(context.Background())
	if err != nil {
		t.Fatalf("Remotes: %v", err)
	}
	want := []Remote{{Name: "acme/widgets", URL: "ssh://farrier@forge.example.com:22/data/git/acme/widgets.git"}}
	if !remotesEqual(remotes, want) {
		t.Fatalf("Remotes = %+v, want %+v", remotes, want)
	}
}

func TestSSHGitExporterEmptyOutput(t *testing.T) {
	runner := &fakeRunner{stdout: ""}
	exporter := &SSHGitExporter{Runner: runner, User: "farrier", Host: "forge.example.com", Root: "/data/git"}

	remotes, err := exporter.Remotes(context.Background())
	if err != nil {
		t.Fatalf("Remotes: %v", err)
	}
	if len(remotes) != 0 {
		t.Fatalf("Remotes = %+v, want empty", remotes)
	}
}

func TestSSHGitExporterQuotesRoot(t *testing.T) {
	runner := &fakeRunner{stdout: ""}
	exporter := &SSHGitExporter{Runner: runner, User: "farrier", Host: "forge.example.com", Root: "/data/git; rm -rf /"}

	if _, err := exporter.Remotes(context.Background()); err != nil {
		t.Fatalf("Remotes: %v", err)
	}
	if !strings.HasPrefix(runner.command, "find '/data/git; rm -rf ' ") {
		t.Fatalf("command = %q, want the root single-quoted as one shell word", runner.command)
	}
}

func TestSSHGitExporterRunError(t *testing.T) {
	runErr := errors.New("boom")
	runner := &fakeRunner{stderr: "find: permission denied\n", err: runErr}
	exporter := &SSHGitExporter{Runner: runner, User: "farrier", Host: "forge.example.com", Root: "/data/git"}

	_, err := exporter.Remotes(context.Background())
	if err == nil {
		t.Fatal("Remotes succeeded despite Runner error, want error")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error = %v, want it to include the remote's stderr", err)
	}
}

func TestSSHGitExporterRequiresFields(t *testing.T) {
	cases := []struct {
		name     string
		exporter *SSHGitExporter
	}{
		{"no runner", &SSHGitExporter{User: "farrier", Host: "h", Root: "/data/git"}},
		{"no host", &SSHGitExporter{Runner: &fakeRunner{}, User: "farrier", Root: "/data/git"}},
		{"no root", &SSHGitExporter{Runner: &fakeRunner{}, User: "farrier", Host: "h"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.exporter.Remotes(context.Background()); err == nil {
				t.Fatal("Remotes succeeded, want error")
			}
		})
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func remotesEqual(got, want []Remote) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
