package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/state"
)

// fakeRunner records the command it was asked to run and answers with a
// canned stdout/stderr/error, standing in for *orchestrate.Client in tests —
// mirrors state.fakeRunner, kept local to this package the same way
// shellQuote is (git.go's doc comment on gitShellQuote).
type fakeRunner struct {
	command string
	stdout  []byte
	stderr  string
	err     error
}

func (f *fakeRunner) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	f.command = command
	if stdout != nil {
		stdout.Write(f.stdout)
	}
	if stderr != nil {
		io.WriteString(stderr, f.stderr)
	}
	return f.err
}

func TestLocalGitCapturerArchivesDirectory(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "HEAD"), "ref: refs/heads/main\n")
	mustWriteFile(t, filepath.Join(root, "objects", "pack", "pack-x.pack"), "pack-bytes")
	mustWriteFile(t, filepath.Join(root, "refs", "heads", "main"), "deadbeef\n")

	capturer := LocalGitCapturer{}
	rc, err := capturer.Archive(context.Background(), state.Remote{Name: "acme/widgets", URL: root})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	defer rc.Close()

	got := readTarFiles(t, rc)
	want := map[string]string{
		"HEAD":                     "ref: refs/heads/main\n",
		"objects/pack/pack-x.pack": "pack-bytes",
		"refs/heads/main":          "deadbeef\n",
	}
	for name, content := range want {
		if got[name] != content {
			t.Errorf("tar entry %q = %q, want %q", name, got[name], content)
		}
	}
	if len(got) != len(want) {
		t.Errorf("tar entries = %v, want exactly %v", got, want)
	}
}

func TestLocalGitCapturerMissingDirectory(t *testing.T) {
	capturer := LocalGitCapturer{}
	_, err := capturer.Archive(context.Background(), state.Remote{Name: "acme/widgets", URL: filepath.Join(t.TempDir(), "missing")})
	if err == nil {
		t.Fatal("Archive: want error for a missing directory, got nil")
	}
}

func TestLocalGitCapturerRejectsNonDirectory(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "not-a-dir")
	mustWriteFile(t, file, "x")

	capturer := LocalGitCapturer{}
	_, err := capturer.Archive(context.Background(), state.Remote{Name: "acme/widgets", URL: file})
	if err == nil {
		t.Fatal("Archive: want error when URL is not a directory, got nil")
	}
}

func TestSSHGitCapturerRunsTarInRemoteDirectory(t *testing.T) {
	runner := &fakeRunner{stdout: []byte("fake-tar-bytes")}
	capturer := SSHGitCapturer{Runner: runner}

	remote := state.Remote{Name: "acme/widgets", URL: "ssh://farrier@forge.example.com:2222/data/git/acme/widgets.git"}
	rc, err := capturer.Archive(context.Background(), remote)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if string(got) != "fake-tar-bytes" {
		t.Fatalf("archive content = %q, want %q", got, "fake-tar-bytes")
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := "tar -C '/data/git/acme/widgets.git' -cf - ."
	if runner.command != want {
		t.Fatalf("command = %q, want %q", runner.command, want)
	}
}

func TestSSHGitCapturerRequiresRunner(t *testing.T) {
	capturer := SSHGitCapturer{}
	_, err := capturer.Archive(context.Background(), state.Remote{Name: "acme/widgets", URL: "ssh://farrier@forge.example.com/data/git/acme/widgets.git"})
	if err == nil {
		t.Fatal("Archive: want error with no runner, got nil")
	}
}

func TestSSHGitCapturerRejectsNonSSHURL(t *testing.T) {
	capturer := SSHGitCapturer{Runner: &fakeRunner{}}
	_, err := capturer.Archive(context.Background(), state.Remote{Name: "acme/widgets", URL: "/local/path"})
	if err == nil {
		t.Fatal("Archive: want error for a non-ssh:// remote URL, got nil")
	}
}

func TestSSHGitCapturerPropagatesRunnerError(t *testing.T) {
	runner := &fakeRunner{err: errors.New("exit status 1"), stderr: "no such directory"}
	capturer := SSHGitCapturer{Runner: runner}

	remote := state.Remote{Name: "acme/widgets", URL: "ssh://farrier@forge.example.com/data/git/acme/widgets.git"}
	rc, err := capturer.Archive(context.Background(), remote)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	defer rc.Close()

	_, readErr := io.ReadAll(rc)
	if readErr == nil {
		t.Fatal("read archive: want error, got nil")
	}
	if !strings.Contains(readErr.Error(), "no such directory") {
		t.Fatalf("error = %q, want it to carry the runner's stderr", readErr.Error())
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// readTarFiles reads every regular-file entry from a tar stream into a
// name -> content map, skipping directory entries.
func readTarFiles(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	out := make(map[string]string)
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar entry: %v", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, tr); err != nil {
			t.Fatalf("read tar entry %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = buf.String()
	}
	return out
}
