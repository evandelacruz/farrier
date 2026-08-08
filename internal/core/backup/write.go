package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/events"
)

// StepWrite identifies the write-to-destination step in a backup job's
// event stream.
const StepWrite = "write"

// Write streams the age-encrypted archive Encrypt wrote at archivePath
// (BKUP-003) to dest — the blob.Adapter OpenDestination resolves from
// `backup --to <uri>` — under the key SnapshotKey(timestamp) names,
// completing BKUP-005: writing the snapshot to an S3-compatible URI or a
// filesystem path. It returns the key it wrote.
//
// Like Run and Encrypt, Write emits its own StepWrite event on job but does
// not end job: a backup job composes Run, Encrypt, and Write (and, once it
// lands, BKUP-004's verification) under one job whose terminal event the
// caller owns.
func Write(ctx context.Context, job *events.Job, dest blob.Adapter, archivePath string, timestamp time.Time) (string, error) {
	job.Started(StepWrite, "writing snapshot to destination")

	if dest == nil {
		return failWrite(job, errors.New("backup: write: destination is required"))
	}
	if strings.TrimSpace(archivePath) == "" {
		return failWrite(job, errors.New("backup: write: archive path is required"))
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return failWrite(job, fmt.Errorf("backup: write: open %s: %w", archivePath, err))
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return failWrite(job, fmt.Errorf("backup: write: stat %s: %w", archivePath, err))
	}

	key := SnapshotKey(timestamp)
	if err := dest.Put(ctx, key, f, info.Size()); err != nil {
		return failWrite(job, fmt.Errorf("backup: write: put %s: %w", key, err))
	}

	job.Emit(StepWrite, events.StateSucceeded, fmt.Sprintf("snapshot written to %s", key))
	return key, nil
}

// failWrite emits a StateFailed StepWrite event and returns err unchanged.
// It does not end job (see Write's doc comment) — the caller's backup job
// owns that.
func failWrite(job *events.Job, err error) (string, error) {
	job.Emit(StepWrite, events.StateFailed, err.Error())
	return "", err
}
