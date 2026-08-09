package drill

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/initialize"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// TestDrillSucceedsFromBundleCopiedToAnotherDirectory closes XCUT-001's
// remaining note by extending the relocated-bundle proof — already made for
// up, backup, restore, promote, and upgrade — to `drill`, the last
// operation that consumes a loaded bundle. Everything Drill resolves from
// one (the keystore driver the snapshot's key material is installed into,
// the blob adapter blobs are restored into, and the age identity that
// decrypts the snapshot) must keep working once the bundle directory itself
// has been physically relocated after being saved.
//
// As with the other proofs, the keystore and blob directories the relocated
// bundle's manifest names (absolute per the XCUT-001 fix) stay put
// throughout; only the bundle directory moves. That is what XCUT-001 claims:
// every operation works from any machine holding the bundle *and* key
// access, not that key access travels with the bundle directory.
func TestDrillSucceedsFromBundleCopiedToAnotherDirectory(t *testing.T) {
	keysDir := t.TempDir()

	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	if err := os.WriteFile(filepath.Join(keysDir, initialize.KeyAgeBackupKey), []byte(identity.String()), 0o600); err != nil {
		t.Fatalf("seed age backup key: %v", err)
	}
	values := testKeyValues()
	for _, name := range []string{state.KeyTLSCertificate, state.KeyTLSPrivateKey} {
		if err := os.WriteFile(filepath.Join(keysDir, name), []byte(values[name]), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	original := testBundle(t, keysDir)

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

	// Rebuilt exactly as internal/api/drill.go's runDrill builds them in
	// production: fresh drivers constructed from the relocated bundle's own
	// manifest, plus the age identity resolved back out through the
	// keystore driver those config values produce.
	keystoreDriver, err := keystore.New(relocated.Manifest.Drivers.Keystore.Driver, relocated.Manifest.Drivers.Keystore.Config)
	if err != nil {
		t.Fatalf("build keystore driver: %v", err)
	}
	blobAdapter, err := blob.New(relocated.Manifest.Drivers.Blob.Driver, relocated.Manifest.Drivers.Blob.Config)
	if err != nil {
		t.Fatalf("build blob driver: %v", err)
	}
	resolvedIdentity, err := backup.ResolveIdentity(ctx, keystoreDriver)
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}

	// The snapshot Drill fetches and decrypts must be encrypted under the
	// same identity ResolveIdentity just resolved from the relocated
	// bundle's keystore, exactly as a real snapshot for this bundle would be.
	destDir := t.TempDir()
	wantKey := buildSnapshotIn(t, destDir, resolvedIdentity)
	source, err := blob.NewLocal(destDir)
	if err != nil {
		t.Fatalf("open snapshot source: %v", err)
	}

	host := newFakeHost()
	job := events.NewJob()
	report, err := Drill(ctx, job, Options{
		RemoteDir:  "/opt/farrier-drill",
		WorkDir:    filepath.Join(t.TempDir(), "work"),
		Bundle:     relocated,
		Source:     source,
		Identity:   resolvedIdentity,
		Keystore:   keystoreDriver,
		Blobs:      blobAdapter,
		Host:       host,
		CertIssuer: fakeCertIssuer{},
	})
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	if !job.Done() {
		t.Fatal("Drill did not end the job")
	}
	if report.SnapshotKey != wantKey {
		t.Errorf("report.SnapshotKey = %q, want %q", report.SnapshotKey, wantKey)
	}

	// The full stack booted on the scratch target: deploy.Up ran as the
	// last stage of the restore Drill sequences.
	if len(host.commandsContaining("docker compose up -d --remove-orphans")) != 1 {
		t.Errorf("want exactly one converge command, got %v", host.commands)
	}

	// Key material landed in the relocated bundle's own (unmoved) keystore
	// directory, resolvable through a driver built fresh from the same
	// config the relocated manifest carries.
	if _, err := keystoreDriver.Resolve(ctx, state.KeyTLSCertificate); err != nil {
		t.Errorf("resolve installed TLS certificate: %v", err)
	}

	// Blobs restored into the relocated bundle's own (unmoved) blob adapter.
	objs, err := blobAdapter.List(ctx, "")
	if err != nil {
		t.Fatalf("list restored blobs: %v", err)
	}
	if len(objs) == 0 {
		t.Errorf("no blobs restored into target blob adapter")
	}
}

// copyBundleDir physically copies src to dst, the same way a bundle
// directory moves to another machine: file bytes only, no directory-entry
// metadata that could carry a hidden reference back to src. Reimplemented
// here (rather than shared with deploy's, backup's, restore's, promote's,
// and upgrade's own) since all are unexported to their own packages.
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
