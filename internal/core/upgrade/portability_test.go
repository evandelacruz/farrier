package upgrade

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// TestUpgradeSucceedsFromBundleCopiedToAnotherDirectory extends
// deploy.TestUpSucceedsFromBundleCopiedToAnotherDirectory,
// backup.TestBackupSucceedsFromBundleCopiedToAnotherDirectory,
// restore.TestRestoreSucceedsFromBundleCopiedToAnotherDirectory, and
// promote.TestPromoteSucceedsFromBundleCopiedToAnotherDirectory's proof to
// the `upgrade` core path (XCUT-001's remaining note): everything Upgrade
// resolves from a loaded bundle -- the keystore driver the pre-upgrade
// backup and the health checks read through, the blob adapter the backup
// captures from -- must keep working, and bumpVersion's own Save must keep
// writing to the right place, once the bundle directory itself has been
// physically relocated after being saved.
//
// As with the other four proofs, the keystore and blob directories the
// relocated bundle's manifest names (absolute per the XCUT-001 fix) stay
// put throughout; only the bundle directory moves.
func TestUpgradeSucceedsFromBundleCopiedToAnotherDirectory(t *testing.T) {
	keysDir := t.TempDir()
	values := testKeyValues()
	for name, value := range values {
		if err := os.WriteFile(filepath.Join(keysDir, name), []byte(value), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	blobsDir := t.TempDir()

	original := testBundle(t, keysDir, blobsDir)

	srcDir := filepath.Join(t.TempDir(), "machine-a", "bundle")
	if err := original.Save(srcDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	dstDir := filepath.Join(t.TempDir(), "machine-b", "elsewhere", "bundle")
	if err := copyBundleDir(t, srcDir, dstDir); err != nil {
		t.Fatalf("copyBundleDir: %v", err)
	}
	if err := os.RemoveAll(filepath.Dir(srcDir)); err != nil {
		t.Fatalf("remove original bundle tree: %v", err)
	}

	relocated, err := bundle.Load(dstDir)
	if err != nil {
		t.Fatalf("Load(%s): %v", dstDir, err)
	}

	ctx := context.Background()

	// Rebuilt exactly as internal/api/upgrade.go's runUpgrade builds them
	// in production: fresh drivers constructed from the relocated bundle's
	// own manifest.
	keystoreDriver, err := keystore.New(relocated.Manifest.Drivers.Keystore.Driver, relocated.Manifest.Drivers.Keystore.Config)
	if err != nil {
		t.Fatalf("build keystore driver: %v", err)
	}
	blobAdapter, err := blob.New(relocated.Manifest.Drivers.Blob.Driver, relocated.Manifest.Drivers.Blob.Config)
	if err != nil {
		t.Fatalf("build blob driver: %v", err)
	}

	opts := Options{
		BundleDir:   dstDir,
		RemoteDir:   "/opt/farrier",
		WorkDir:     filepath.Join(t.TempDir(), "work"),
		Bundle:      relocated,
		Destination: filepath.Join(t.TempDir(), "backups"),
		NewImage:    "codeberg.org/forgejo/forgejo:1.22",
		Identity:    mustAgeIdentity(t),
		Keystore:    keystoreDriver,
		Blobs:       blobAdapter,
		Host:        newFakeHost(testDatabaseBytes(t)),
		CertIssuer:  fakeCertIssuer{},
		Resolver:    &fakeResolver{},
	}

	job := events.NewJob()
	if err := Upgrade(ctx, job, opts); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if !job.Done() {
		t.Fatal("Upgrade did not end the job")
	}

	// The bumped manifest was saved to the relocated directory, not the
	// original (now-deleted) one.
	reloaded, err := bundle.Load(dstDir)
	if err != nil {
		t.Fatalf("reload relocated bundle: %v", err)
	}
	wantImage := "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("f", 64)
	if got := reloaded.Manifest.Images[forge.Service]; got != wantImage {
		t.Errorf("saved forgejo image = %q, want %q", got, wantImage)
	}

	// The pre-upgrade backup captured key material through the relocated
	// bundle's own (unmoved) keystore, and blobs through its own (unmoved)
	// blob adapter — both resolved fresh from dstDir's manifest above.
	source, err := blob.NewLocal(opts.Destination)
	if err != nil {
		t.Fatalf("open destination: %v", err)
	}
	if _, err := backup.LatestSnapshotKey(ctx, source); err != nil {
		t.Fatalf("LatestSnapshotKey: %v", err)
	}
}

// copyBundleDir physically copies src to dst, the same way a bundle
// directory moves to another machine: file bytes only, no directory-entry
// metadata that could carry a hidden reference back to src. Reimplemented
// here (rather than shared with deploy's, backup's, restore's, and
// promote's own copyDir / copyBundleDir) since all are unexported to their
// own packages.
func copyBundleDir(t *testing.T, src, dst string) error {
	t.Helper()
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
}
