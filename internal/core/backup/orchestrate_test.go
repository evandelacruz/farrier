package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"

	"github.com/evandelacruz/farrier/internal/core/events"
)

// validOptions builds an Options that, unmodified, runs a complete backup
// successfully: the same fakes validParams(t) already assembles for Run,
// plus a filesystem destination, scratch work directory, and a fresh age
// identity.
func validOptions(t *testing.T) Options {
	t.Helper()
	params := validParams(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	return Options{
		WorkDir:         filepath.Join(t.TempDir(), "work"),
		ForgejoVersion:  params.ForgejoVersion,
		Destination:     filepath.Join(t.TempDir(), "destination"),
		Identity:        identity,
		Git:             params.Git,
		GitCapturer:     params.GitCapturer,
		Database:        params.Database,
		Blobs:           params.Blobs,
		Keys:            params.Keys,
		PushHold:        params.PushHold,
		PushHoldCeiling: params.PushHoldCeiling,
	}
}

func TestBackupEndToEnd(t *testing.T) {
	opts := validOptions(t)
	job := events.NewJob()

	if err := Backup(context.Background(), job, opts); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	assertJobSucceeded(t, job)

	// The archive landed at the destination, named by SnapshotKey.
	entries, err := os.ReadDir(opts.Destination)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("destination has %d entries, want 1: %v", len(entries), entries)
	}
	if filepath.Ext(entries[0].Name()) != ".age" {
		t.Errorf("destination object %q does not look like a snapshot archive", entries[0].Name())
	}

	// The plaintext work directory is gone.
	if _, err := os.Stat(opts.WorkDir); !os.IsNotExist(err) {
		t.Errorf("WorkDir still exists after a successful Backup: %v", err)
	}

	// The archive actually decrypts with the identity Backup used, and
	// nothing in it is plaintext key material (KEY-003).
	data, err := os.ReadFile(filepath.Join(opts.Destination, entries[0].Name()))
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	got := decryptAndUntar(t, data, opts.Identity)
	if got["keys/secret_key"] != "sk-value" {
		t.Errorf("keys/secret_key = %q, want %q", got["keys/secret_key"], "sk-value")
	}

	// Every step Backup itself owns, plus every step Run, Encrypt, and
	// Write relay through, reached a terminal event on the one external
	// job — never left "started" forever, and never duplicated as a
	// second job-terminal event.
	wantSteps := []string{
		StepResolveDestination,
		StepValidate, StepPushHold, StepDatabase, StepRecordRefs, StepGit, StepBlobs, StepKeys, StepWriteManifest, StepVerify,
		StepEncrypt,
		StepVerifyEncrypted,
		StepWrite,
	}
	started := map[string]int{}
	terminal := map[string]int{}
	for _, ev := range job.Events() {
		if ev.Step == "" {
			continue
		}
		switch ev.State {
		case events.StateStarted:
			started[ev.Step]++
		case events.StateSucceeded, events.StateFailed:
			terminal[ev.Step]++
		}
	}
	for _, step := range wantSteps {
		if started[step] != 1 {
			t.Errorf("step %s started %d time(s), want 1", step, started[step])
		}
		if terminal[step] != 1 {
			t.Errorf("step %s reached a terminal event %d time(s), want 1", step, terminal[step])
		}
	}
}

func TestBackupMissingWorkDir(t *testing.T) {
	opts := validOptions(t)
	opts.WorkDir = ""
	job := events.NewJob()

	if err := Backup(context.Background(), job, opts); err == nil {
		t.Fatal("Backup: want error for missing work directory, got nil")
	}
	assertJobFailed(t, job)
}

func TestBackupMissingIdentity(t *testing.T) {
	opts := validOptions(t)
	opts.Identity = nil
	job := events.NewJob()

	if err := Backup(context.Background(), job, opts); err == nil {
		t.Fatal("Backup: want error for missing identity, got nil")
	}
	assertJobFailed(t, job)
}

func TestBackupResolveDestinationFails(t *testing.T) {
	opts := validOptions(t)
	opts.Destination = ""
	job := events.NewJob()

	if err := Backup(context.Background(), job, opts); err == nil {
		t.Fatal("Backup: want error for an unresolvable destination, got nil")
	}
	assertJobFailed(t, job)
	assertOneFailedStep(t, job, StepResolveDestination)

	if _, err := os.Stat(opts.WorkDir); !os.IsNotExist(err) {
		t.Errorf("WorkDir still exists after a failed Backup: %v", err)
	}
}

func TestBackupCaptureFails(t *testing.T) {
	opts := validOptions(t)
	opts.Database = &fakeDatabaseExporter{err: errors.New("database unreachable")}
	job := events.NewJob()

	if err := Backup(context.Background(), job, opts); err == nil {
		t.Fatal("Backup: want error when capture fails, got nil")
	}
	assertJobFailed(t, job)
	assertOneFailedStep(t, job, StepDatabase)

	if _, err := os.Stat(opts.WorkDir); !os.IsNotExist(err) {
		t.Errorf("WorkDir still exists after a failed Backup: %v", err)
	}
}

func TestBackupCaptureFailsLeavesNoObjectAtDestination(t *testing.T) {
	opts := validOptions(t)
	opts.Database = &fakeDatabaseExporter{err: errors.New("database unreachable")}
	job := events.NewJob()

	if err := Backup(context.Background(), job, opts); err == nil {
		t.Fatal("Backup: want error, got nil")
	}

	entries, err := os.ReadDir(opts.Destination)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read destination: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("destination has %d entries after a failed backup, want 0: %v", len(entries), entries)
	}
}

func TestDecryptArchiveRoundTrips(t *testing.T) {
	dir := writeSnapshotFixture(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	archivePath := filepath.Join(t.TempDir(), "snapshot.age")
	job := events.NewJob()
	if err := Encrypt(context.Background(), job, dir, archivePath, identity.Recipient()); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	destDir := t.TempDir()
	if err := decryptArchive(context.Background(), archivePath, destDir, identity); err != nil {
		t.Fatalf("decryptArchive: %v", err)
	}

	want := map[string]string{
		"snapshot-manifest.json": `{"forgejoVersion":"11.0.2"}`,
		"db.sqlite":              "sqlite-bytes",
		"repos/acme/widgets.tar": "tar-bytes",
		"keys/secret_key":        "sk-value",
	}
	for rel, content := range want {
		got, err := os.ReadFile(filepath.Join(destDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read extracted %s: %v", rel, err)
		}
		if string(got) != content {
			t.Errorf("extracted %s = %q, want %q", rel, got, content)
		}
	}
}

func TestDecryptArchiveWrongIdentity(t *testing.T) {
	dir := writeSnapshotFixture(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	wrongIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	archivePath := filepath.Join(t.TempDir(), "snapshot.age")
	job := events.NewJob()
	if err := Encrypt(context.Background(), job, dir, archivePath, identity.Recipient()); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	if err := decryptArchive(context.Background(), archivePath, t.TempDir(), wrongIdentity); err == nil {
		t.Fatal("decryptArchive: want error decrypting with the wrong identity, got nil")
	}
}
