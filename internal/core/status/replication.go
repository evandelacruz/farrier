// Package status implements the `status` command's core logic.
// This file: replication lag for golden-path transports (STAT-002). See
// status.go for instance health (STAT-001: services, TLS, disk); STAT-001's
// last-backup age is not yet implemented.
package status

import (
	"context"
	"fmt"
	"time"

	"github.com/evandelacruz/farrier/internal/core/state"
)

// LagState says whether ReplicationLag was able to measure lag for a
// destination, and why not when it wasn't.
type LagState string

const (
	// LagUnmeasured means there is nothing Farrier configured to inspect:
	// either no golden-path destination exists yet, or the destination is
	// an operator-assembled transport outside the system's own
	// measurement (spec.md "Replication lag") — the caller passes a nil
	// destination for both cases, since this package has no way to tell
	// them apart from a blob.Adapter alone. It is also what a golden-path
	// destination yields when every object it holds has an unknown
	// Modified time (an older third-party exec adapter that predates the
	// field), since that is indistinguishable from "no data" without
	// guessing.
	LagUnmeasured LagState = "unmeasured"
	// LagNoBackups means a golden-path destination is configured and
	// reachable but holds no objects yet.
	LagNoBackups LagState = "no-backups"
	// LagMeasured means LastBackup and Age both hold a real measurement.
	LagMeasured LagState = "measured"
)

// Lag is what `status` reports for one destination.
type Lag struct {
	State LagState
	// LastBackup is the newest object's Modified time. Valid only when
	// State == LagMeasured.
	LastBackup time.Time
	// Age is now minus LastBackup, clamped to zero. Valid only when State
	// == LagMeasured.
	Age time.Duration
	// Skew is positive when LastBackup is after now — the destination's
	// clock reads ahead of the reporting clock, which is what a negative
	// Age would otherwise have signaled silently. Zero when there's no
	// skew. Valid only when State == LagMeasured.
	Skew time.Duration
}

// ReplicationLag reports replication lag for one golden-path destination
// (spec.md "Replication lag"): dest is the read side of the blob.Adapter
// `backup --to` (BKUP-005) writes snapshots to. now is the reporting time,
// passed in rather than read from the clock so callers get a deterministic
// result.
//
// A nil dest always yields LagUnmeasured: there is no bundle-level
// destination config to invent here, so a caller with no golden-path
// destination — because the operator hasn't set one up, or because they
// run their own replication topology outside the system — reports
// unmeasured rather than guessing. Once `backup --to` exists, the caller
// that resolves its destination into a blob.Adapter starts passing it here
// and lag reporting lights up with no further change to this function.
//
// When dest is non-nil, lag is the age of the newest object across every
// object dest holds. No objects at all is LagNoBackups: the destination is
// real, it simply has nothing in it yet. Objects present but none with a
// known Modified time is LagUnmeasured, not a fabricated zero lag.
func ReplicationLag(ctx context.Context, dest state.BlobExporter, now time.Time) (Lag, error) {
	if dest == nil {
		return Lag{State: LagUnmeasured}, nil
	}

	objects, err := dest.List(ctx, "")
	if err != nil {
		return Lag{}, fmt.Errorf("status: replication lag: %w", err)
	}
	if len(objects) == 0 {
		return Lag{State: LagNoBackups}, nil
	}

	var newest time.Time
	for _, o := range objects {
		if o.Modified.After(newest) {
			newest = o.Modified
		}
	}
	if newest.IsZero() {
		return Lag{State: LagUnmeasured}, nil
	}

	age := now.Sub(newest)
	var skew time.Duration
	if age < 0 {
		skew, age = -age, 0
	}
	return Lag{
		State:      LagMeasured,
		LastBackup: newest,
		Age:        age,
		Skew:       skew,
	}, nil
}
