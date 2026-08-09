package orchestrate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunStdinStreamsToRemoteCommand(t *testing.T) {
	client := dialShellServer(t)
	target := filepath.Join(t.TempDir(), "file.txt")

	var stdout, stderr bytes.Buffer
	err := client.RunStdin(t.Context(), "cat > "+shQuote(target), strings.NewReader("payload"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("RunStdin: %v (stderr: %s)", err, stderr.String())
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("content = %q, want %q", got, "payload")
	}
}

func TestRunStdinCapturesStdout(t *testing.T) {
	client := dialShellServer(t)

	var stdout bytes.Buffer
	if err := client.RunStdin(t.Context(), "cat", strings.NewReader("echoed"), &stdout, nil); err != nil {
		t.Fatalf("RunStdin: %v", err)
	}
	if stdout.String() != "echoed" {
		t.Errorf("stdout = %q, want %q", stdout.String(), "echoed")
	}
}

func TestRunStdinPropagatesNonZeroExit(t *testing.T) {
	client := dialShellServer(t)

	var stderr bytes.Buffer
	err := client.RunStdin(t.Context(), "cat >/dev/null; echo boom >&2; exit 3", strings.NewReader(""), nil, &stderr)
	if err == nil {
		t.Fatal("RunStdin: want error for a nonzero exit, got nil")
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Errorf("stderr = %q, want it to carry boom", stderr.String())
	}
}
