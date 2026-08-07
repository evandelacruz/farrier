package orchestrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutputReturnsStdout(t *testing.T) {
	client := dialShellServer(t)

	got, err := client.Output(t.Context(), "printf hello")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("Output = %q, want %q", got, "hello")
	}
}

// TestOutputFailureIncludesStderr covers the reason Output exists as its own
// method: it discards the stderr writer, so unless the message carries
// stderr a caller has only an exit status to go on.
func TestOutputFailureIncludesStderr(t *testing.T) {
	client := dialShellServer(t)

	_, err := client.Output(t.Context(), "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("Output: want error, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want it to carry stderr %q", err, "boom")
	}
}

func TestWriteFileCreatesParentsAndAppliesMode(t *testing.T) {
	client := dialShellServer(t)
	target := filepath.Join(t.TempDir(), "sub", "nested", "file.txt")

	if err := client.WriteFile(t.Context(), target, []byte("payload"), 0o640); err != nil {
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

	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("staging file left behind: err = %v", err)
	}
}

// TestWriteFileContentIsNotShellQuoted guards the reason content travels
// over stdin instead of being interpolated into the command: a payload full
// of quotes, backticks, and $ must land byte for byte, not be re-expanded
// by the remote shell.
func TestWriteFileContentIsNotShellQuoted(t *testing.T) {
	client := dialShellServer(t)
	target := filepath.Join(t.TempDir(), "hostile.txt")
	content := "it's `whoami` $HOME \"quoted\" \\ end\n"

	if err := client.WriteFile(t.Context(), target, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != content {
		t.Errorf("content = %q, want %q", got, content)
	}
}

// TestWriteFilePathIsShellQuoted covers the other half: the path itself is
// interpolated into the command, so a directory with a space in it must not
// split into two arguments.
func TestWriteFilePathIsShellQuoted(t *testing.T) {
	client := dialShellServer(t)
	target := filepath.Join(t.TempDir(), "dir with spaces", "file.txt")

	if err := client.WriteFile(t.Context(), target, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("content = %q, want %q", got, "payload")
	}
}

// TestWriteFileLeavesNoPartialFileOnFailure is the atomicity contract:
// the target must never appear when the write cannot complete.
func TestWriteFileLeavesNoPartialFileOnFailure(t *testing.T) {
	client := dialShellServer(t)
	dir := t.TempDir()

	// A regular file where WriteFile needs a directory, so mkdir -p fails.
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	target := filepath.Join(blocker, "file.txt")

	if err := client.WriteFile(t.Context(), target, []byte("payload"), 0o644); err == nil {
		t.Fatal("WriteFile: want error, got nil")
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("target exists after a failed write")
	}
	if got, err := os.ReadFile(blocker); err != nil || string(got) != "x" {
		t.Errorf("blocker = %q (err %v), want it untouched", got, err)
	}
}

// TestWriteFileReplacesExistingFile covers the overwrite path — the case a
// second converge hits for every file it ships.
func TestWriteFileReplacesExistingFile(t *testing.T) {
	client := dialShellServer(t)
	target := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := client.WriteFile(t.Context(), target, []byte("new"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
}

// TestClientSatisfiesTransport is the whole point of ORCH-002 riding on
// ORCH-001's landed client: Converge takes a Transport, and *Client must be
// one without an adapter in between.
func TestClientSatisfiesTransport(t *testing.T) {
	var _ Transport = (*Client)(nil)
}
