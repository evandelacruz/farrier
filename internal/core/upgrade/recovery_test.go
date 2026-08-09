package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// TestUpgradeFailureLeavesAWorkingPathBack is UPGR-002 itself: an upgrade
// that fails at any step after the pre-upgrade backup must leave that
// backup intact, verifiable, and still pinning the pre-upgrade Forgejo
// version — and must name it, and the command that restores it, on both
// the failure and the CORE-002 event stream.
//
// The three cases are the three places the sequence can fail once the
// backup is on disk, and they differ in how much of the instance has
// already moved: nothing (the resolver never answered), the host mid-flight
// (deploy.Up failed), and an instance that did restart on the new image and
// migrated its schema (the post-upgrade health check failed). The path back
// has to hold in all three.
func TestUpgradeFailureLeavesAWorkingPathBack(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{
			name:   "image resolver fails",
			mutate: func(o *Options) { o.Resolver = &fakeResolver{err: errors.New("registry unreachable")} },
		},
		{
			name: "deploy fails",
			mutate: func(o *Options) {
				o.Host.(*fakeHost).failCommand = convergeCommand
			},
		},
		{
			name: "upgraded instance fails verification",
			mutate: func(o *Options) {
				o.Host.(*fakeHost).downAfterConverge = true
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, _ := validOptions(t)
			preUpgradeVersion := opts.Bundle.Manifest.Images[forge.Service]
			tt.mutate(&opts)

			job := events.NewJob()
			err := Upgrade(context.Background(), job, opts)
			if err == nil {
				t.Fatal("Upgrade: want an error, got nil")
			}

			// The failure names the snapshot, where it is, the version it
			// pins, and the command that restores it.
			key := onlySnapshotKey(t, opts.Destination)
			for _, want := range []string{key, opts.Destination, preUpgradeVersion, "farrier restore"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Upgrade error does not mention %q:\n%s", want, err.Error())
				}
			}

			// The same path back reached the event stream, so the
			// dashboard shows it too and it is not only in terminal
			// scrollback.
			detail, ok := recoveryEvent(job)
			if !ok {
				t.Fatalf("no %s event on the job: %v", StepRecoveryPath, job.Events())
			}
			for _, want := range []string{key, opts.Destination, preUpgradeVersion, "farrier restore"} {
				if !strings.Contains(detail, want) {
					t.Errorf("%s event does not mention %q: %s", StepRecoveryPath, want, detail)
				}
			}
			if !strings.Contains(detail, opts.Host.Target().String()) {
				t.Errorf("%s event does not name the restore target: %s", StepRecoveryPath, detail)
			}

			// And the snapshot it points at is really there, really
			// verifies, and really pins the pre-upgrade version — the
			// three things that make it a path back rather than a
			// reference to something gone.
			manifest := fetchAndVerifySnapshot(t, opts, key)
			if manifest.ForgejoVersion != preUpgradeVersion {
				t.Errorf("snapshot ForgejoVersion = %q, want the pre-upgrade pin %q", manifest.ForgejoVersion, preUpgradeVersion)
			}
		})
	}
}

// TestUpgradeSuccessEmitsNoRecoveryPath keeps the recovery event to the
// failure paths: a successful upgrade has no path back to advertise, and
// an operator who sees one has been told something untrue about what just
// happened.
func TestUpgradeSuccessEmitsNoRecoveryPath(t *testing.T) {
	opts, _ := validOptions(t)

	job := events.NewJob()
	if err := Upgrade(context.Background(), job, opts); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if detail, ok := recoveryEvent(job); ok {
		t.Errorf("successful upgrade emitted a %s event: %s", StepRecoveryPath, detail)
	}
}

// TestUpgradeBackupFailureHasNoRecoveryPath covers the other side: if the
// pre-upgrade backup itself fails there is no snapshot from this run to
// point at, and nothing has changed on the host either, so naming a path
// back would name one that does not exist.
func TestUpgradeBackupFailureHasNoRecoveryPath(t *testing.T) {
	opts, bundleDir := validOptions(t)
	opts.Host.(*fakeHost).failCommand = "sqlite3"

	job := events.NewJob()
	err := Upgrade(context.Background(), job, opts)
	if err == nil {
		t.Fatal("Upgrade: want an error when the pre-upgrade backup fails, got nil")
	}
	if detail, ok := recoveryEvent(job); ok {
		t.Errorf("failed backup still emitted a %s event: %s", StepRecoveryPath, detail)
	}

	reloaded, loadErr := bundle.Load(bundleDir)
	if loadErr != nil {
		t.Fatalf("reload bundle: %v", loadErr)
	}
	if reloaded.Manifest.Images[forge.Service] != opts.Bundle.Manifest.Images[forge.Service] {
		t.Error("bundle manifest was bumped despite the pre-upgrade backup failing")
	}
}

// TestValidateRefusesDestinationInsideWorkDir guards the one configuration
// that would delete the pre-upgrade snapshot on the way out: Upgrade wipes
// its work directory on every exit path, so a destination nested inside it
// would take the snapshot with it exactly when UPGR-002 needs it most.
func TestValidateRefusesDestinationInsideWorkDir(t *testing.T) {
	workDir := t.TempDir()

	tests := []struct {
		name        string
		destination string
		wantRefused bool
	}{
		{"nested under the work directory", filepath.Join(workDir, "backups"), true},
		{"the work directory itself", workDir, true},
		{"deeply nested", filepath.Join(workDir, "a", "b", "c"), true},
		{"a sibling directory", filepath.Join(filepath.Dir(workDir), "backups"), false},
		{"an s3 uri", "s3://bucket/snapshots", false},
		{"a path that merely shares a prefix", workDir + "-backups", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, _ := validOptions(t)
			opts.WorkDir = workDir
			opts.Destination = tt.destination

			err := opts.validate()
			if tt.wantRefused {
				if err == nil {
					t.Fatalf("validate: want a refusal for destination %s, got nil", tt.destination)
				}
				if !strings.Contains(err.Error(), "work directory") {
					t.Errorf("validate error = %q, want it to explain the work directory", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("validate: unexpected error for destination %s: %v", tt.destination, err)
			}
		})
	}
}

func TestRecoveryCommand(t *testing.T) {
	r := Recovery{
		Destination:    "/srv/backups",
		SnapshotKey:    "20250101T000000Z.age",
		ForgejoVersion: "codeberg.org/forgejo/forgejo@sha256:abc",
		Target:         "ssh://root@forge.example.com:22",
		BundleDir:      "/home/ops/bundle",
	}
	want := "farrier restore -bundle /home/ops/bundle -target ssh://root@forge.example.com:22 -from /srv/backups -snapshot 20250101T000000Z.age"
	if got := r.Command(); got != want {
		t.Errorf("Command() = %q, want %q", got, want)
	}
	if !strings.Contains(r.Detail(), want) {
		t.Errorf("Detail() does not carry the command: %s", r.Detail())
	}
	if strings.Contains(r.Detail(), "\n") {
		t.Errorf("Detail() should stay on one line for the event stream: %q", r.Detail())
	}
}

// recoveryEvent returns the detail of the job's StepRecoveryPath event.
func recoveryEvent(job *events.Job) (string, bool) {
	for _, ev := range job.Events() {
		if ev.Step == StepRecoveryPath {
			return ev.Detail, true
		}
	}
	return "", false
}

// onlySnapshotKey asserts destination holds exactly one snapshot — the
// pre-upgrade one — and returns its key. A failed upgrade must not have
// written a second.
func onlySnapshotKey(t *testing.T, destination string) string {
	t.Helper()
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("destination holds %d snapshot(s), want exactly the pre-upgrade one", len(entries))
	}
	return entries[0].Name()
}

// fetchAndVerifySnapshot fetches key from opts.Destination, decrypts it
// with the bundle's age identity, and runs the same backup.Verify pass
// restore runs before it will proceed (RSTR-003) — proving the snapshot
// named in the recovery path is one restore would actually accept.
func fetchAndVerifySnapshot(t *testing.T, opts Options, key string) *backup.Manifest {
	t.Helper()
	ctx := context.Background()

	source, err := blob.NewLocal(opts.Destination)
	if err != nil {
		t.Fatalf("open destination: %v", err)
	}
	archivePath := filepath.Join(t.TempDir(), "snapshot.age")
	if err := backup.Fetch(ctx, source, key, archivePath); err != nil {
		t.Fatalf("Fetch %s: %v", key, err)
	}
	plainDir := t.TempDir()
	if err := backup.DecryptArchive(ctx, archivePath, plainDir, opts.Identity); err != nil {
		t.Fatalf("DecryptArchive: %v", err)
	}
	manifest, err := backup.ReadManifest(plainDir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	keys := &state.KeystoreKeyExporter{Driver: opts.Keystore}
	if err := backup.Verify(ctx, plainDir, manifest, keys.Names()); err != nil {
		t.Fatalf("Verify the pre-upgrade snapshot: %v", err)
	}
	return manifest
}
