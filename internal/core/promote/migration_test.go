package promote

import (
	"context"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
)

// TestPromoteDoesNotMigrate is UPGR-003 for the failover path. Promote
// composes restore.Restore, so it boots the version the snapshot pins rather
// than the bundle's own (RSTR-002) — the standby is brought up on exactly
// the version that wrote the database being placed on it, which is why
// Forgejo has nothing to migrate. This asserts that composition holds
// end to end, including on a standby that was previously up on a different
// version, which is the case where a bundle-pinned promote would migrate.
func TestPromoteDoesNotMigrate(t *testing.T) {
	opts := validOptions(t)
	host := opts.Host.(*fakeHost)

	snapshotImage := "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("c", 64)
	bundleImage := opts.Bundle.Manifest.Images[forge.Service]
	if snapshotImage == bundleImage {
		t.Fatal("fixture no longer distinguishes the snapshot's forgejo image from the bundle's")
	}

	versionPath := deploy.StateVersionPath(opts.RemoteDir)
	host.files[versionPath] = bundleImage + "\n"

	if err := Promote(context.Background(), events.NewJob(), opts); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	if got := strings.TrimSpace(host.files[versionPath]); got != snapshotImage {
		t.Fatalf("recorded forge version after promote = %q, want the snapshot's %q", got, snapshotImage)
	}
	for _, shipped := range host.files {
		if strings.Contains(shipped, bundleImage) {
			t.Fatalf("promote shipped the bundle's own forgejo image %q; the snapshot's %q is the one that must run", bundleImage, snapshotImage)
		}
	}
}
