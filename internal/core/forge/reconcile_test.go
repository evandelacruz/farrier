package forge

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/events"

	_ "modernc.org/sqlite"
)

// openTestDB creates a fresh SQLite file at t.TempDir()/gitea.db with the
// two Forgejo Actions tables ReconcileCI touches, sized down to just the
// columns it reads or writes.
func openTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gitea.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { db.Close() })

	schema := []string{
		`CREATE TABLE action_run (id INTEGER PRIMARY KEY, status INTEGER, updated INTEGER)`,
		`CREATE TABLE action_run_job (id INTEGER PRIMARY KEY, run_id INTEGER, status INTEGER, updated INTEGER)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	return db, path
}

func insertRun(t *testing.T, db *sql.DB, id, status int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO action_run (id, status, updated) VALUES (?, ?, 1000)`, id, status); err != nil {
		t.Fatalf("insert action_run: %v", err)
	}
}

func insertJob(t *testing.T, db *sql.DB, id, runID, status int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO action_run_job (id, run_id, status, updated) VALUES (?, ?, ?, 1000)`, id, runID, status); err != nil {
		t.Fatalf("insert action_run_job: %v", err)
	}
}

func statusOf(t *testing.T, db *sql.DB, table string, id int) int {
	t.Helper()
	var status int
	if err := db.QueryRow("SELECT status FROM "+table+" WHERE id = ?", id).Scan(&status); err != nil {
		t.Fatalf("query %s status for id %d: %v", table, id, err)
	}
	return status
}

func TestReconcileCIResetsRunningToQueued(t *testing.T) {
	db, path := openTestDB(t)
	insertRun(t, db, 1, actionStatusRunning)
	insertRun(t, db, 2, actionStatusRunning)
	insertJob(t, db, 1, 1, actionStatusRunning)
	insertJob(t, db, 2, 1, actionStatusRunning)
	insertJob(t, db, 3, 2, actionStatusRunning)

	result, err := ReconcileCI(context.Background(), nil, path)
	if err != nil {
		t.Fatalf("ReconcileCI: %v", err)
	}
	if result.RunsReset != 2 {
		t.Errorf("RunsReset = %d, want 2", result.RunsReset)
	}
	if result.JobsReset != 3 {
		t.Errorf("JobsReset = %d, want 3", result.JobsReset)
	}

	for _, id := range []int{1, 2} {
		if got := statusOf(t, db, "action_run", id); got != actionStatusWaiting {
			t.Errorf("action_run %d status = %d, want %d (waiting/queued)", id, got, actionStatusWaiting)
		}
	}
	for _, id := range []int{1, 2, 3} {
		if got := statusOf(t, db, "action_run_job", id); got != actionStatusWaiting {
			t.Errorf("action_run_job %d status = %d, want %d (waiting/queued)", id, got, actionStatusWaiting)
		}
	}
}

func TestReconcileCILeavesOtherStatusesAlone(t *testing.T) {
	db, path := openTestDB(t)
	const (
		statusSuccess   = 1
		statusFailure   = 2
		statusCancelled = 3
		statusSkipped   = 4
	)
	insertRun(t, db, 1, statusSuccess)
	insertRun(t, db, 2, actionStatusWaiting)
	insertJob(t, db, 1, 1, statusFailure)
	insertJob(t, db, 2, 1, statusCancelled)
	insertJob(t, db, 3, 2, statusSkipped)
	insertJob(t, db, 4, 2, actionStatusWaiting)

	result, err := ReconcileCI(context.Background(), nil, path)
	if err != nil {
		t.Fatalf("ReconcileCI: %v", err)
	}
	if result.RunsReset != 0 || result.JobsReset != 0 {
		t.Errorf("result = %+v, want zero resets — nothing was running", result)
	}

	if got := statusOf(t, db, "action_run", 1); got != statusSuccess {
		t.Errorf("action_run 1 status = %d, want unchanged %d", got, statusSuccess)
	}
	if got := statusOf(t, db, "action_run", 2); got != actionStatusWaiting {
		t.Errorf("action_run 2 status = %d, want unchanged %d", got, actionStatusWaiting)
	}
	wantJob := map[int]int{1: statusFailure, 2: statusCancelled, 3: statusSkipped, 4: actionStatusWaiting}
	for id, want := range wantJob {
		if got := statusOf(t, db, "action_run_job", id); got != want {
			t.Errorf("action_run_job %d status = %d, want unchanged %d", id, got, want)
		}
	}
}

func TestReconcileCIEmitsStepEvents(t *testing.T) {
	_, path := openTestDB(t)
	job := events.NewJob()

	if _, err := ReconcileCI(context.Background(), job, path); err != nil {
		t.Fatalf("ReconcileCI: %v", err)
	}

	got := job.Events()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (started, succeeded): %+v", len(got), got)
	}
	if got[0].State != events.StateStarted || got[0].Step != StepCIReconcile {
		t.Errorf("event 0 = %+v, want step=%s state=started", got[0], StepCIReconcile)
	}
	last := got[len(got)-1]
	if last.State != events.StateSucceeded || last.Step != StepCIReconcile {
		t.Errorf("event 1 = %+v, want step=%s state=succeeded", last, StepCIReconcile)
	}
	if job.Done() {
		t.Error("ReconcileCI ended the job; it should leave the terminal event to the caller")
	}
}

func TestReconcileCIMissingTablesEmitsFailedStep(t *testing.T) {
	job := events.NewJob()
	// sql.Open + modernc.org/sqlite create the file lazily on first access
	// rather than erroring on a missing path, so the failure this exercises
	// is the actions schema being absent — table not found — not a missing
	// file.
	blank := filepath.Join(t.TempDir(), "blank.db")

	_, err := ReconcileCI(context.Background(), job, blank)
	if err == nil {
		t.Fatal("ReconcileCI succeeded against a database with no actions tables, want error")
	}

	got := job.Events()
	last := got[len(got)-1]
	if last.State != events.StateFailed || last.Step != StepCIReconcile {
		t.Errorf("last event = %+v, want step=%s state=failed", last, StepCIReconcile)
	}
	if job.Done() {
		t.Error("ReconcileCI ended the job on a step failure; it should leave the terminal event to the caller")
	}
}

func TestReconcileCIRunsAndJobsAreIndependent(t *testing.T) {
	// A run can be running while its jobs have already finished dispatch
	// bookkeeping (or vice versa) — ReconcileCI must reset each table on
	// its own terms rather than assuming they move in lockstep.
	db, path := openTestDB(t)
	insertRun(t, db, 1, actionStatusRunning)
	insertJob(t, db, 1, 1, actionStatusWaiting)

	result, err := ReconcileCI(context.Background(), nil, path)
	if err != nil {
		t.Fatalf("ReconcileCI: %v", err)
	}
	if result.RunsReset != 1 || result.JobsReset != 0 {
		t.Errorf("result = %+v, want RunsReset=1 JobsReset=0", result)
	}
}
