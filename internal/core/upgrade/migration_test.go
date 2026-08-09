package upgrade

import (
	"context"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
)

// UPGR-003's other half: `upgrade` is the one path that may start Forgejo on
// a version other than the one the host's state was last started under, and
// so the one path that migrates. deploy.Up refuses that everywhere else
// (internal/core/deploy's own stateversion_test.go); here the requirement is
// that upgrade is not refused, and that it leaves the record naming the
// version it actually started.

func TestUpgradeIsTheOnePathThatMigrates(t *testing.T) {
	opts, _ := validOptions(t)
	host := opts.Host.(*fakeHost)

	versionPath := deploy.StateVersionPath(opts.RemoteDir)
	preUpgrade := opts.Bundle.Manifest.Images[forge.Service]
	host.files[versionPath] = preUpgrade + "\n"

	if err := Upgrade(context.Background(), events.NewJob(), opts); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}

	// The record now names the bumped image — the version Forgejo was just
	// started on, and so the version its schema was migrated to.
	wantImage := "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("f", 64)
	if got := strings.TrimSpace(host.files[versionPath]); got != wantImage {
		t.Fatalf("recorded forge version after upgrade = %q, want the bumped %q", got, wantImage)
	}
	if len(host.commandsContaining(convergeCommand)) != 1 {
		t.Fatalf("want exactly one converge command, got %v", host.commands)
	}
}

// TestUpgradeRecordsTheBumpedVersionBeforeConverging pins the ordering the
// record's meaning depends on. Converge is what restarts Forgejo on the new
// image, and that restart is the migration; a record written afterward would
// leave a half-finished upgrade looking, to the next command that reads it,
// like a host still on the old version.
func TestUpgradeRecordsTheBumpedVersionBeforeConverging(t *testing.T) {
	opts, _ := validOptions(t)
	host := opts.Host.(*fakeHost)
	host.failCommand = convergeCommand

	if err := Upgrade(context.Background(), events.NewJob(), opts); err == nil {
		t.Fatal("Upgrade succeeded with a failing converge; want error")
	}

	wantImage := "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("f", 64)
	got := strings.TrimSpace(host.files[deploy.StateVersionPath(opts.RemoteDir)])
	if got != wantImage {
		t.Fatalf("recorded forge version after a failed converge = %q, want the bumped %q", got, wantImage)
	}
}
