package restore

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/state"

	"filippo.io/age"
)

// forgeActionStatusRunning and forgeActionStatusWaiting mirror the
// unexported action-status values forge/reconcile.go's ReconcileCI reads
// and writes (actionStatusRunning, actionStatusWaiting) — this package
// can't import forge's unexported constants, so the two fixed integers
// (Forgejo Actions' own status enum) are reproduced here.
const (
	forgeActionStatusRunning = 6
	forgeActionStatusWaiting = 5
)

// newTestDatabaseBytesWithRunningCI extends newTestDatabaseBytes' fixture
// with the two Forgejo Actions tables forge.ReconcileCI touches
// (forge/reconcile_test.go's openTestDB schema), seeded with run/job 1 left
// `running` — an orphan a snapshot captured mid-flight, the case
// Options.ReconcileCI exists to clean up — and run/job 2 already `queued`,
// dispatch bookkeeping the snapshot captured mid-flight in the other valid
// state. Both must reach the standby host still dispatchable: 1 by being
// reset, 2 by being left alone (FAIL-003).
func newTestDatabaseBytesWithRunningCI(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gitea.db")
	if err := os.WriteFile(path, newTestDatabaseBytes(t, [][2]string{{"acme", "widgets"}}), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	stmts := []string{
		`CREATE TABLE action_run (id INTEGER PRIMARY KEY, status INTEGER, updated INTEGER)`,
		`CREATE TABLE action_run_job (id INTEGER PRIMARY KEY, run_id INTEGER, status INTEGER, updated INTEGER)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO action_run (id, status, updated) VALUES (1, ?, 1000)`, forgeActionStatusRunning); err != nil {
		t.Fatalf("seed action_run: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO action_run_job (id, run_id, status, updated) VALUES (1, 1, ?, 1000)`, forgeActionStatusRunning); err != nil {
		t.Fatalf("seed action_run_job: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO action_run (id, status, updated) VALUES (2, ?, 1000)`, forgeActionStatusWaiting); err != nil {
		t.Fatalf("seed action_run: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO action_run_job (id, run_id, status, updated) VALUES (2, 2, ?, 1000)`, forgeActionStatusWaiting); err != nil {
		t.Fatalf("seed action_run_job: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// buildSnapshotWithRunningCI is buildSnapshot, reimplemented here (same
// unexported-fakes reason as buildSnapshot's own doc comment) with a
// database component that carries an orphaned running run and job, so a
// test can exercise Options.ReconcileCI end to end.
func buildSnapshotWithRunningCI(t *testing.T) (blob.Adapter, *age.X25519Identity) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	destDir := t.TempDir()

	opts := backup.Options{
		WorkDir:        filepath.Join(t.TempDir(), "work"),
		ForgejoVersion: testSnapshotForgeImage,
		Destination:    destDir,
		Identity:       identity,
		Git:            &fakeGitExporter{remotes: []state.Remote{{Name: "acme/widgets"}}},
		GitCapturer:    fakeGitCapturer{},
		Database:       &fakeDatabaseExporter{data: newTestDatabaseBytesWithRunningCI(t)},
		Blobs:          mustLocalBlob(t, t.TempDir()),
		Keys:           &fakeKeyExporter{values: testKeyValues()},
		PushHold:       backup.NoopPushHold{},
	}
	job := events.NewJob()
	if _, err := backup.Backup(context.Background(), job, opts); err != nil {
		t.Fatalf("build snapshot: Backup: %v", err)
	}

	source, err := blob.NewLocal(destDir)
	if err != nil {
		t.Fatalf("open snapshot source: %v", err)
	}
	return source, identity
}

// seedTLSKeys installs the TLS certificate/key pair testKeyValues()
// captured into keysDir, matching validOptions' own setup — deploy.Up's
// configureTLS step (run at the end of Restore) needs a persisted
// certificate to reuse, since fakeCertIssuer refuses to invent one.
func seedTLSKeys(t *testing.T, keysDir string) {
	t.Helper()
	values := testKeyValues()
	for _, name := range []string{state.KeyTLSCertificate, state.KeyTLSPrivateKey} {
		if err := os.WriteFile(filepath.Join(keysDir, name), []byte(values[name]), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
}

func TestRestoreReconcileCIResetsRunningJobsBeforePlacement(t *testing.T) {
	source, identity := buildSnapshotWithRunningCI(t)
	opts := optionsWithSnapshot(t, source, identity)
	opts.ReconcileCI = true
	seedTLSKeys(t, opts.Bundle.Manifest.Drivers.Keystore.Config["path"].(string))

	host := opts.Host.(*fakeHost)
	job := events.NewJob()
	if err := Restore(context.Background(), job, opts); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// The database streamed to the host must already carry the reset
	// status: ReconcileCI has to run against the local snapshot file before
	// placeState ships it, since forge.ReconcileCI has no remote-SQL path.
	shippedPath := filepath.Join(t.TempDir(), "shipped.db")
	if err := os.WriteFile(shippedPath, host.stdinFor(t, "cat > '/opt/farrier/state/gitea/gitea.db'"), 0o600); err != nil {
		t.Fatalf("write shipped db: %v", err)
	}
	db, err := sql.Open("sqlite", shippedPath)
	if err != nil {
		t.Fatalf("open shipped db: %v", err)
	}
	defer db.Close()

	// FAIL-003: every run/job the standby host's forgejo container reads on
	// startup must already be dispatchable — id 1 by having been reset from
	// its orphaned `running` state, id 2 by having been left alone since it
	// was already `queued` when the snapshot was taken. Both went through
	// the real backup encryption and restore decryption round trip, not a
	// hand-built database file.
	for _, id := range []int{1, 2} {
		var runStatus, jobStatus int
		if err := db.QueryRow(`SELECT status FROM action_run WHERE id = ?`, id).Scan(&runStatus); err != nil {
			t.Fatalf("query action_run %d status: %v", id, err)
		}
		if runStatus != forgeActionStatusWaiting {
			t.Errorf("shipped action_run %d status = %d, want %d (queued)", id, runStatus, forgeActionStatusWaiting)
		}
		if err := db.QueryRow(`SELECT status FROM action_run_job WHERE id = ?`, id).Scan(&jobStatus); err != nil {
			t.Fatalf("query action_run_job %d status: %v", id, err)
		}
		if jobStatus != forgeActionStatusWaiting {
			t.Errorf("shipped action_run_job %d status = %d, want %d (queued)", id, jobStatus, forgeActionStatusWaiting)
		}
	}

	reconcileIdx, placeIdx := -1, -1
	for i, ev := range job.Events() {
		if reconcileIdx == -1 && ev.Step == forge.StepCIReconcile && ev.State == events.StateStarted {
			reconcileIdx = i
		}
		if placeIdx == -1 && ev.Step == StepPlaceState && ev.State == events.StateStarted {
			placeIdx = i
		}
	}
	if reconcileIdx == -1 {
		t.Fatal("no CI reconcile step event found")
	}
	if placeIdx == -1 {
		t.Fatal("no place-state step event found")
	}
	if reconcileIdx > placeIdx {
		t.Errorf("CI reconcile step (event %d) ran after place-state (event %d), want before", reconcileIdx, placeIdx)
	}
}

func TestRestoreDefaultDoesNotReconcileCI(t *testing.T) {
	source, identity := buildSnapshotWithRunningCI(t)
	opts := optionsWithSnapshot(t, source, identity)
	// ReconcileCI left at its zero value (false): a plain restore's
	// existing behavior must be unchanged.
	seedTLSKeys(t, opts.Bundle.Manifest.Drivers.Keystore.Config["path"].(string))

	job := events.NewJob()
	if err := Restore(context.Background(), job, opts); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	for _, ev := range job.Events() {
		if ev.Step == forge.StepCIReconcile {
			t.Fatalf("unexpected CI reconcile event on a plain restore: %+v", ev)
		}
	}
}
