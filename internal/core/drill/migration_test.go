package drill

import (
	"context"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
)

// TestDrillDoesNotMigrate is UPGR-003 for the rehearsal path, and the case
// that matters most: a scratch target is the host most likely to have been
// booted before on some other version, since drills reuse it. Drill composes
// restore.Restore, which places the snapshot's database and boots the exact
// version that wrote it (RSTR-002), so a drill never migrates — and a drill
// that did would be rehearsing something other than the restore it exists to
// rehearse.
func TestDrillDoesNotMigrate(t *testing.T) {
	f := newFixture(t)
	host := f.host()

	snapshotImage := "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("c", 64)
	bundleImage := f.opts.Bundle.Manifest.Images[forge.Service]
	if snapshotImage == bundleImage {
		t.Fatal("fixture no longer distinguishes the snapshot's forgejo image from the bundle's")
	}

	versionPath := deploy.StateVersionPath(f.opts.RemoteDir)
	host.files[versionPath] = bundleImage + "\n"

	report, err := Drill(context.Background(), events.NewJob(), f.opts)
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	if !report.Succeeded() {
		t.Fatalf("drill failed: %v", report.Failure)
	}

	if got := strings.TrimSpace(host.files[versionPath]); got != snapshotImage {
		t.Fatalf("recorded forge version after drill = %q, want the snapshot's %q", got, snapshotImage)
	}
	for _, shipped := range host.files {
		if strings.Contains(shipped, bundleImage) {
			t.Fatalf("drill shipped the bundle's own forgejo image %q; the snapshot's %q is the one that must run", bundleImage, snapshotImage)
		}
	}
}
