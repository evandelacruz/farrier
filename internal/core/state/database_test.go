package state

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeSQLite3 writes a stand-in "sqlite3" executable that understands
// exactly the invocation LocalDatabaseExporter and SSHDatabaseExporter make
// — `sqlite3 <db> ".backup '<dest>'"` — and copies db's bytes to dest,
// standing in for the real Online Backup API the same way dns_test.go's
// rfc2136 tests substitute `tee` for nsupdate.
func writeFakeSQLite3(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-sqlite3.sh")
	script := `#!/bin/sh
set -e
db="$1"
dotcmd="$2"
dest=$(printf '%s' "$dotcmd" | sed -n "s/^\.backup '\(.*\)'\$/\1/p")
if [ -z "$dest" ]; then
  echo "fake-sqlite3: could not parse dest from: $dotcmd" >&2
  exit 1
fi
cat "$db" > "$dest"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake sqlite3: %v", err)
	}
	return path
}

// writeFailingSQLite3 writes a stand-in "sqlite3" that always fails,
// carrying a distinctive stderr message so tests can assert it surfaces.
func writeFailingSQLite3(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-sqlite3-fail.sh")
	script := `#!/bin/sh
echo "disk I/O error" >&2
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write failing fake sqlite3: %v", err)
	}
	return path
}

func TestLocalDatabaseExporterSnapshot(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "gitea.db")
	content := []byte("pretend sqlite database contents")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("write source db: %v", err)
	}

	exporter := &LocalDatabaseExporter{Path: src, Command: writeFakeSQLite3(t, dir)}

	rc, err := exporter.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("snapshot content = %q, want %q", got, content)
	}

	tf, ok := rc.(*tempFileReadCloser)
	if !ok {
		t.Fatalf("Snapshot returned %T, want *tempFileReadCloser", rc)
	}
	tmpPath := tf.path

	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("temp snapshot file %s still exists after Close", tmpPath)
	}
}

func TestLocalDatabaseExporterRequiresPath(t *testing.T) {
	exporter := &LocalDatabaseExporter{}
	if _, err := exporter.Snapshot(context.Background()); err == nil {
		t.Fatal("Snapshot succeeded with no path, want error")
	}
}

func TestLocalDatabaseExporterCommandFailure(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "gitea.db")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatalf("write source db: %v", err)
	}

	exporter := &LocalDatabaseExporter{Path: src, Command: writeFailingSQLite3(t, dir)}

	_, err := exporter.Snapshot(context.Background())
	if err == nil {
		t.Fatal("Snapshot succeeded, want error")
	}
	if !strings.Contains(err.Error(), "disk I/O error") {
		t.Fatalf("error = %q, want it to carry the command's stderr", err.Error())
	}
}

func TestSSHDatabaseExporterSnapshot(t *testing.T) {
	content := "pretend sqlite database contents"
	runner := &fakeRunner{stdout: content}
	exporter := &SSHDatabaseExporter{Runner: runner, Container: "farrier-forge", Path: "/data/gitea/gitea.db"}

	rc, err := exporter.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if string(got) != content {
		t.Fatalf("snapshot content = %q, want %q", got, content)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, want := range []string{
		"docker exec 'farrier-forge' sqlite3 '/data/gitea/gitea.db'",
		"docker exec 'farrier-forge' cat ",
		"docker exec 'farrier-forge' rm -f ",
	} {
		if !strings.Contains(runner.command, want) {
			t.Fatalf("command = %q, want it to contain %q", runner.command, want)
		}
	}
}

func TestSSHDatabaseExporterRequiresFields(t *testing.T) {
	cases := []struct {
		name     string
		exporter *SSHDatabaseExporter
	}{
		{"no runner", &SSHDatabaseExporter{Container: "farrier-forge", Path: "/data/gitea/gitea.db"}},
		{"no container", &SSHDatabaseExporter{Runner: &fakeRunner{}, Path: "/data/gitea/gitea.db"}},
		{"no path", &SSHDatabaseExporter{Runner: &fakeRunner{}, Container: "farrier-forge"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.exporter.Snapshot(context.Background()); err == nil {
				t.Fatalf("Snapshot succeeded with %s, want error", tc.name)
			}
		})
	}
}

func TestSSHDatabaseExporterRunnerFailure(t *testing.T) {
	runner := &fakeRunner{err: errors.New("exit status 1"), stderr: "no such container"}
	exporter := &SSHDatabaseExporter{Runner: runner, Container: "farrier-forge", Path: "/data/gitea/gitea.db"}

	_, err := exporter.Snapshot(context.Background())
	if err == nil {
		t.Fatal("Snapshot succeeded, want error")
	}
	if !strings.Contains(err.Error(), "no such container") {
		t.Fatalf("error = %q, want it to carry the runner's stderr", err.Error())
	}
}

func TestSSHDatabaseExporterUsesDistinctTempPaths(t *testing.T) {
	runner := &fakeRunner{stdout: "x"}
	exporter := &SSHDatabaseExporter{Runner: runner, Container: "farrier-forge", Path: "/data/gitea/gitea.db"}

	rc1, err := exporter.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot 1: %v", err)
	}
	cmd1 := runner.command
	rc1.Close()

	rc2, err := exporter.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot 2: %v", err)
	}
	cmd2 := runner.command
	rc2.Close()

	if cmd1 == cmd2 {
		t.Fatal("two Snapshot calls built identical commands, want distinct container-side temp paths")
	}
}

func TestBackupDotCommandQuotesEmbeddedSingleQuote(t *testing.T) {
	got := backupDotCommand("/tmp/it's-a-path.db")
	want := fmt.Sprintf(".backup '%s'", `/tmp/it'\''s-a-path.db`)
	if got != want {
		t.Fatalf("backupDotCommand = %q, want %q", got, want)
	}
}
