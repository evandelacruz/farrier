package backup

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/caddy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/initialize"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// TestBackupSucceedsFromBundleCopiedToAnotherDirectory extends
// deploy.TestUpSucceedsFromBundleCopiedToAnotherDirectory's proof to the
// `backup` CLI/core path (XCUT-001's remaining note): everything BuildOptions
// resolves from a loaded bundle — the keystore driver, the blob adapter, and
// the age backup key ResolveIdentity reads through it — must work from a
// bundle physically relocated after the directory it was saved to no longer
// exists.
//
// The bundle directory itself is moved, exactly as the `up` proof does. The
// keystore and blob storage it points at (via absolute config.path, per the
// XCUT-001 fix rejecting relative ones) stay put throughout: XCUT-001 is
// that every operation works from any machine holding the bundle *and* key
// access, not that key access travels with the bundle directory.
func TestBackupSucceedsFromBundleCopiedToAnotherDirectory(t *testing.T) {
	keysDir := t.TempDir()
	blobsDir := t.TempDir()

	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	if err := os.WriteFile(filepath.Join(keysDir, initialize.KeyAgeBackupKey), []byte(identity.String()), 0o600); err != nil {
		t.Fatalf("seed age backup key: %v", err)
	}
	// captureKeys resolves the fixed set state.KeyExporter.Names() returns
	// (every key a bundle carries besides the age backup key itself), so
	// each needs a value in the keystore for Backup to succeed.
	for _, name := range (&state.KeystoreKeyExporter{}).Names() {
		if err := os.WriteFile(filepath.Join(keysDir, name), []byte("fake-"+name+"-value"), 0o600); err != nil {
			t.Fatalf("seed key %s: %v", name, err)
		}
	}

	original := &bundle.Bundle{
		Manifest: *bundle.NewManifest("example.com", map[string]string{
			forge.Service: "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("a", 64),
			caddy.Service: "docker.io/library/caddy@sha256:" + strings.Repeat("b", 64),
		}, bundle.DriverConfig{
			Keystore: bundle.DriverRef{Driver: "file", Config: map[string]any{"path": keysDir}},
			Blob:     bundle.DriverRef{Driver: "local", Config: map[string]any{"path": blobsDir}},
		}, bundle.ACMEConfig{DNSProvider: "manual", Email: "ops@example.com"}),
		Compose: map[string][]byte{
			"docker-compose.yml": []byte("services:\n  forgejo:\n    image: x\n  caddy:\n    image: y\n"),
		},
	}

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

	keystoreDriver, err := keystore.New(relocated.Manifest.Drivers.Keystore.Driver, relocated.Manifest.Drivers.Keystore.Config)
	if err != nil {
		t.Fatalf("build keystore driver: %v", err)
	}
	blobAdapter, err := blob.New(relocated.Manifest.Drivers.Blob.Driver, relocated.Manifest.Drivers.Blob.Config)
	if err != nil {
		t.Fatalf("build blob driver: %v", err)
	}
	resolvedIdentity, err := ResolveIdentity(ctx, keystoreDriver)
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}

	host := &fakePortabilityHost{target: orchestrate.Target{User: "git", Host: "forge.example.com", Port: "22"}}
	destDir := t.TempDir()
	opts := BuildOptions(host, relocated, "/opt/farrier", filepath.Join(t.TempDir(), "work"), destDir, resolvedIdentity, blobAdapter, keystoreDriver)

	// Git, GitCapturer, Database, and PushHold are BuildOptions' SSH-backed
	// fields: they depend only on the live target host, never on the bundle
	// directory, so — like TestUpSucceedsFromBundleCopiedToAnotherDirectory's
	// fakeHost — they're swapped for simple fakes here rather than a fake SSH
	// transcript, keeping the proof scoped to what relocation can actually
	// break: Blobs and Keys, both built above from the relocated bundle's own
	// driver config.
	opts.Git = &fakeGitExporter{}
	opts.GitCapturer = &fakeGitCapturer{}
	opts.Database = &fakeDatabaseExporter{data: newTestDatabaseBytes(t, nil, nil, nil)}
	opts.PushHold = &fakePushHold{}

	job := events.NewJob()
	if _, err := Backup(ctx, job, opts); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	assertJobSucceeded(t, job)

	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("destination has %d entries, want 1: %v", len(entries), entries)
	}
}

// copyBundleDir physically copies src to dst, the same way a bundle
// directory moves to another machine: file bytes only, no directory-entry
// metadata that could carry a hidden reference back to src.
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

// fakePortabilityHost implements Host (state.Runner + Target) without a
// real SSH connection. It succeeds every command trivially: the relocated
// bundle proof exercises the parts of the backup path that are actually
// sensitive to where the bundle directory lives — the keystore driver, the
// blob adapter, and identity resolution — not the SSH-backed git and
// database capture, which depend only on the live target host, never on the
// bundle's own directory.
type fakePortabilityHost struct {
	target orchestrate.Target
}

func (f *fakePortabilityHost) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	return nil
}

func (f *fakePortabilityHost) Target() orchestrate.Target { return f.target }
