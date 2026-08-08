package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"

	"github.com/evandelacruz/farrier/internal/core/events"
)

// writeSnapshotFixture creates a small directory tree standing in for the
// plain snapshot Run produces, and returns its root.
func writeSnapshotFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"snapshot-manifest.json": `{"forgejoVersion":"11.0.2"}`,
		"db.sqlite":              "sqlite-bytes",
		"repos/acme/widgets.tar": "tar-bytes",
		"keys/secret_key":        "sk-value",
	}
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return dir
}

// decryptAndUntar decrypts data with identity and returns its tar entries'
// contents keyed by name, failing the test on any error.
func decryptAndUntar(t *testing.T, data []byte, identity age.Identity) map[string]string {
	t.Helper()
	plain, err := age.Decrypt(bytes.NewReader(data), identity)
	if err != nil {
		t.Fatalf("age.Decrypt: %v", err)
	}
	tr := tar.NewReader(plain)
	got := make(map[string]string)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, tr); err != nil {
			t.Fatalf("read tar entry %s: %v", hdr.Name, err)
		}
		got[hdr.Name] = buf.String()
	}
	return got
}

func TestEncryptRoundTrips(t *testing.T) {
	dir := writeSnapshotFixture(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	destPath := filepath.Join(t.TempDir(), "out", "snapshot.age")
	job := events.NewJob()

	if err := Encrypt(context.Background(), job, dir, destPath, identity.Recipient()); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	data, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read %s: %v", destPath, err)
	}
	if bytes.Contains(data, []byte("sk-value")) {
		t.Error("encrypted archive contains a plaintext secret (KEY-003)")
	}

	got := decryptAndUntar(t, data, identity)
	want := map[string]string{
		"snapshot-manifest.json": `{"forgejoVersion":"11.0.2"}`,
		"db.sqlite":              "sqlite-bytes",
		"repos/acme/widgets.tar": "tar-bytes",
		"keys/secret_key":        "sk-value",
	}
	for name, content := range want {
		if got[name] != content {
			t.Errorf("decrypted entry %s = %q, want %q", name, got[name], content)
		}
	}
}

func TestEncryptRejectsWrongIdentity(t *testing.T) {
	dir := writeSnapshotFixture(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	wrongIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	destPath := filepath.Join(t.TempDir(), "snapshot.age")

	if err := Encrypt(context.Background(), events.NewJob(), dir, destPath, identity.Recipient()); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	data, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read %s: %v", destPath, err)
	}
	if _, err := age.Decrypt(bytes.NewReader(data), wrongIdentity); err == nil {
		t.Fatal("age.Decrypt: want error decrypting with the wrong identity, got nil")
	}
}

func TestEncryptEmitsStepEvents(t *testing.T) {
	dir := writeSnapshotFixture(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	destPath := filepath.Join(t.TempDir(), "snapshot.age")
	job := events.NewJob()

	if err := Encrypt(context.Background(), job, dir, destPath, identity.Recipient()); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	evs := job.Events()
	if len(evs) != 2 {
		t.Fatalf("events = %+v, want exactly 2 (started, succeeded)", evs)
	}
	if evs[0].Step != StepEncrypt || evs[0].State != events.StateStarted {
		t.Errorf("first event = %+v, want a started %s event", evs[0], StepEncrypt)
	}
	if evs[1].Step != StepEncrypt || evs[1].State != events.StateSucceeded {
		t.Errorf("second event = %+v, want a succeeded %s event", evs[1], StepEncrypt)
	}
	if job.Done() {
		t.Error("Encrypt must not end job — a backup job composes further steps after it")
	}
}

func TestEncryptRejectsMissingInputs(t *testing.T) {
	dir := writeSnapshotFixture(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	destPath := filepath.Join(t.TempDir(), "snapshot.age")

	cases := []struct {
		name      string
		dir       string
		destPath  string
		recipient age.Recipient
	}{
		{"missing dir", "", destPath, identity.Recipient()},
		{"missing destPath", dir, "", identity.Recipient()},
		{"missing recipient", dir, destPath, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := events.NewJob()
			if err := Encrypt(context.Background(), job, tc.dir, tc.destPath, tc.recipient); err == nil {
				t.Fatal("Encrypt: want error, got nil")
			}
			evs := job.Events()
			last := evs[len(evs)-1]
			if last.Step != StepEncrypt || last.State != events.StateFailed {
				t.Errorf("last event = %+v, want a failed %s event", last, StepEncrypt)
			}
			if job.Done() {
				t.Error("Encrypt must not end job on a step failure — the caller's backup job owns the terminal event")
			}
		})
	}
}

func TestEncryptPropagatesArchiveError(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	missingDir := filepath.Join(t.TempDir(), "does-not-exist")
	destPath := filepath.Join(t.TempDir(), "snapshot.age")
	job := events.NewJob()

	err = Encrypt(context.Background(), job, missingDir, destPath, identity.Recipient())
	if err == nil {
		t.Fatal("Encrypt: want error for a missing snapshot directory, got nil")
	}
	if _, statErr := os.Stat(destPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("Encrypt left a partial archive at %s on failure", destPath)
	}
}
