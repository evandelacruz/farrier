package promote

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// TestPromoteSucceedsFromBundleCopiedToAnotherDirectory extends
// deploy.TestUpSucceedsFromBundleCopiedToAnotherDirectory,
// backup.TestBackupSucceedsFromBundleCopiedToAnotherDirectory, and
// restore.TestRestoreSucceedsFromBundleCopiedToAnotherDirectory's proof to
// the `promote` core path (XCUT-001's remaining note): everything Promote
// resolves from a loaded target bundle -- the keystore driver key material
// is installed into, the blob adapter blobs are restored into, and the age
// identity that decrypts the fetched snapshot (via the restore.Restore it
// sequences) -- must keep working once the bundle directory itself has been
// physically relocated after being saved.
//
// As with the up, backup, and restore proofs, the keystore and blob
// directories the relocated bundle's manifest names (absolute per the
// XCUT-001 fix) stay put throughout; only the bundle directory moves. That
// matches what XCUT-001 actually claims: every operation works from any
// machine holding the bundle *and* key access, not that key access travels
// with the bundle directory itself.
func TestPromoteSucceedsFromBundleCopiedToAnotherDirectory(t *testing.T) {
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

	// Rebuilt exactly as internal/api/promote.go's runPromote builds them in
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

	// The snapshot Promote's own restore.Restore step fetches and decrypts
	// must be encrypted under the same identity ResolveIdentity just
	// resolved from the relocated bundle's keystore, exactly as a real
	// snapshot for this bundle would be.
	source := buildSnapshotForIdentity(t, resolvedIdentity)

	host := newFakeHost()
	dnsDriver := &fakeDNSDriver{}
	opts := Options{
		RemoteDir:  "/opt/farrier",
		WorkDir:    filepath.Join(t.TempDir(), "work"),
		Bundle:     relocated,
		Source:     source,
		Identity:   resolvedIdentity,
		Keystore:   keystoreDriver,
		Blobs:      blobAdapter,
		Host:       host,
		CertIssuer: fakeCertIssuer{},
		DNS:        dnsDriver,
		DNSValue:   "203.0.113.10",
	}

	job := events.NewJob()
	if err := Promote(ctx, job, opts); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if !job.Done() {
		t.Fatal("Promote did not end the job")
	}

	// deploy.Up ran as the last stage of the restore.Restore Promote
	// sequences: the compose definition was converged on the standby host.
	if len(host.commandsContaining("docker compose up -d --remove-orphans")) != 1 {
		t.Errorf("want exactly one converge command, got %v", host.commands)
	}

	// The DNS flip applied the relocated bundle's own domain.
	if len(dnsDriver.sets) != 1 || dnsDriver.sets[0].record != testDomain {
		t.Fatalf("dns sets = %v, want one Set for %s", dnsDriver.sets, testDomain)
	}

	// Key material landed in the relocated bundle's own (unmoved) keystore
	// directory, resolvable through a driver built fresh from the same
	// config the relocated manifest carries.
	if _, err := keystoreDriver.Resolve(ctx, state.KeyTLSCertificate); err != nil {
		t.Errorf("resolve installed TLS certificate: %v", err)
	}

	// Blobs restored into the relocated bundle's own (unmoved) blob
	// adapter.
	objs, err := blobAdapter.List(ctx, "")
	if err != nil {
		t.Fatalf("list restored blobs: %v", err)
	}
	if len(objs) == 0 {
		t.Errorf("no blobs restored into target blob adapter")
	}
}

// buildSnapshotForIdentity is buildSnapshot (promote_test.go), reimplemented
// to encrypt under a caller-supplied identity rather than generating its
// own: the relocated-bundle proof above needs the snapshot encrypted under
// exactly the identity ResolveIdentity resolves from the relocated bundle's
// own keystore, not an unrelated one.
func buildSnapshotForIdentity(t *testing.T, identity *age.X25519Identity) blob.Adapter {
	t.Helper()
	destDir := t.TempDir()

	blobsSource := mustLocalBlob(t, t.TempDir())
	if err := blobsSource.Put(context.Background(), "avatars/1.png", strings.NewReader("avatar-bytes"), -1); err != nil {
		t.Fatalf("seed source blob: %v", err)
	}

	opts := backup.Options{
		WorkDir:        filepath.Join(t.TempDir(), "work"),
		ForgejoVersion: "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("c", 64),
		Destination:    destDir,
		Identity:       identity,
		Git:            &fakeGitExporter{remotes: []state.Remote{{Name: "acme/widgets"}}},
		GitCapturer:    fakeGitCapturer{},
		Database:       &fakeDatabaseExporter{data: newTestDatabaseBytes(t)},
		Blobs:          blobsSource,
		Keys:           &fakeKeyExporter{values: testKeyValues()},
		PushHold:       backup.NoopPushHold{},
	}
	job := events.NewJob()
	if _, err := backup.Backup(context.Background(), job, opts); err != nil {
		t.Fatalf("build snapshot: Backup: %v", err)
	}

	source, err := blob.NewLocal(destDir)
	if err != nil {
		t.Fatalf("open snapshot source: %v", err)
	}
	return source
}

// copyBundleDir physically copies src to dst, the same way a bundle
// directory moves to another machine: file bytes only, no directory-entry
// metadata that could carry a hidden reference back to src. Reimplemented
// here (rather than shared with deploy's, backup's, and restore's own
// copyDir / copyBundleDir) since all are unexported to their own packages.
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
