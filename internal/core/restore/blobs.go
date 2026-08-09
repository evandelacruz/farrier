package restore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
)

// restoreBlobs puts every blob component the snapshot at plainDir captured
// into target (STATE-003) — the inverse of capture.go's captureBlobs,
// walking the same manifest.Components list and streaming each captured
// file straight through blob.Adapter.Put rather than holding it in memory.
func restoreBlobs(ctx context.Context, job *events.Job, plainDir string, manifest *backup.Manifest, target blob.Adapter) error {
	job.Started(StepRestoreBlobs, "restoring blobs")

	count := 0
	for _, c := range manifest.Components {
		if c.Kind != bundle.StateKindBlobs {
			continue
		}
		if err := ctx.Err(); err != nil {
			job.Emit(StepRestoreBlobs, events.StateFailed, err.Error())
			return err
		}
		if err := restoreOneBlob(ctx, plainDir, c, target); err != nil {
			job.Emit(StepRestoreBlobs, events.StateFailed, err.Error())
			return err
		}
		count++
	}

	job.Emit(StepRestoreBlobs, events.StateSucceeded, fmt.Sprintf("restored %d blob(s)", count))
	return nil
}

func restoreOneBlob(ctx context.Context, plainDir string, c backup.Component, target blob.Adapter) error {
	full := filepath.Join(plainDir, filepath.FromSlash(c.Path))
	f, err := os.Open(full)
	if err != nil {
		return fmt.Errorf("restore: restore blobs: open %s: %w", c.Path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("restore: restore blobs: stat %s: %w", c.Path, err)
	}
	if err := target.Put(ctx, c.Name, f, info.Size()); err != nil {
		return fmt.Errorf("restore: restore blobs: put %s: %w", c.Name, err)
	}
	return nil
}
