package forge

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/evandelacruz/farrier/internal/core/events"

	_ "modernc.org/sqlite"
)

// StepCIReconcile identifies the CI-reconciliation step in a job's event
// stream.
const StepCIReconcile = "ci-reconcile"

// Forgejo Actions status values (models/actions.Status, upstream Gitea and
// Forgejo). statusWaiting is "queued, not yet dispatched to a runner" —
// what the functional requirement calls "queued." statusRunning is
// "dispatched to a runner and executing." FORGE-004 only ever moves
// Running back to Waiting; every other status (success, failure,
// cancelled, skipped, blocked, ...) is left untouched.
const (
	actionStatusWaiting = 5
	actionStatusRunning = 6
)

// ReconcileCIResult reports how many rows ReconcileCI reset.
type ReconcileCIResult struct {
	// RunsReset is the number of action_run rows moved from running to
	// queued.
	RunsReset int64
	// JobsReset is the number of action_run_job rows moved from running to
	// queued.
	JobsReset int64
}

// ReconcileCI resets every Forgejo Actions run and job in the `running`
// state back to `queued`, in the SQLite database at dbPath (FORGE-004).
//
// It operates as a direct SQL update against the database file — the
// forge's own service does not need to be running, and per spec.md
// "Failover" this must execute before services start. A run or job still
// marked running in a restored or promoted database is an orphan: whatever
// runner was executing it does not exist on the new host, or no longer
// holds that work. Resetting it to queued lets Forgejo's own scheduler
// re-dispatch it to a runner in a fresh workspace with no operator action;
// jobs already queued are untouched and dispatch on their own once
// services start.
//
// job may be nil for callers that don't have a job in progress (tests,
// one-off tooling); when non-nil, ReconcileCI emits StepCIReconcile
// started/succeeded/failed events but — matching Bootstrap — never emits
// the job's own terminal event, which stays the caller's responsibility.
func ReconcileCI(ctx context.Context, job *events.Job, dbPath string) (ReconcileCIResult, error) {
	if job != nil {
		job.Started(StepCIReconcile, "reconciling CI: resetting running jobs to queued")
	}

	result, err := reconcileCI(ctx, dbPath)
	if err != nil {
		if job != nil {
			job.Emit(StepCIReconcile, events.StateFailed, fmt.Sprintf("reconcile CI: %s", err))
		}
		return ReconcileCIResult{}, fmt.Errorf("forge: reconcile CI: %w", err)
	}

	if job != nil {
		job.Emit(StepCIReconcile, events.StateSucceeded, fmt.Sprintf(
			"reconciled CI: %d run(s) and %d job(s) reset from running to queued",
			result.RunsReset, result.JobsReset,
		))
	}
	return result, nil
}

func reconcileCI(ctx context.Context, dbPath string) (ReconcileCIResult, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return ReconcileCIResult{}, fmt.Errorf("open %s: %w", dbPath, err)
	}
	defer db.Close()

	now := time.Now().Unix()

	runs, err := resetRunning(ctx, db, "action_run", now)
	if err != nil {
		return ReconcileCIResult{}, err
	}
	jobs, err := resetRunning(ctx, db, "action_run_job", now)
	if err != nil {
		return ReconcileCIResult{}, err
	}

	return ReconcileCIResult{RunsReset: runs, JobsReset: jobs}, nil
}

// resetRunning updates every row in table with status = running to status =
// waiting, and returns how many rows changed. table is always one of the
// two fixed identifiers ReconcileCI passes in — never caller input — so
// building the statement with fmt.Sprintf carries no injection risk.
func resetRunning(ctx context.Context, db *sql.DB, table string, updated int64) (int64, error) {
	stmt := fmt.Sprintf("UPDATE %s SET status = ?, updated = ? WHERE status = ?", table)
	res, err := db.ExecContext(ctx, stmt, actionStatusWaiting, updated, actionStatusRunning)
	if err != nil {
		return 0, fmt.Errorf("reset %s: %w", table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reset %s: rows affected: %w", table, err)
	}
	return n, nil
}
