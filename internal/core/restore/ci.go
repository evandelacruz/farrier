package restore

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
)

// reconcileCI runs forge.ReconcileCI (FORGE-004) against the snapshot's
// decrypted database file at plainDir, before placeState ever ships it to
// the host — the only point restore holds that file locally, which is what
// forge.ReconcileCI's direct SQLite update needs (it has no remote-SQL
// path) and matches tech-spec.md "Forge configuration"'s "executed before
// services start". A run or job the snapshot captured mid-flight is an
// orphan on any host restore places it on — nothing on a fresh or
// failed-over host is still executing it — so resetting it to queued here
// lets Forgejo's own scheduler re-dispatch it once deploy.Up starts the
// forgejo container, with no operator action (FAIL-003).
func reconcileCI(ctx context.Context, job *events.Job, plainDir string, manifest *backup.Manifest) error {
	dbComponent, err := databaseComponent(manifest)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	dbPath := filepath.Join(plainDir, filepath.FromSlash(dbComponent.Path))

	if _, err := forge.ReconcileCI(ctx, job, dbPath); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	return nil
}
