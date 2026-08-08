package deploy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
)

// TestUpSucceedsFromBundleCopiedToAnotherDirectory proves XCUT-001 for the
// `up` sequencing specifically, one layer past CORE-001's narrower claim
// that Bundle.Load/Save round-trip a manifest byte-for-byte
// (bundle_test.go's TestPortability). Here the loaded bundle is actually
// driven through Up: the keystore driver it carries is resolved for real,
// and the resulting sequencing runs against a fake host exactly as it
// would for a bundle that never moved.
//
// The bundle directory is saved to one path, physically copied to a wholly
// different one (standing in for "another machine"), and then deleted at
// its original location before Up ever runs — so nothing about Up's
// success can come from a lingering reference to where the bundle was
// first written. The keystore itself stays at one fixed, absolute path
// throughout, standing in for "given key access": XCUT-001 requires every
// operation to work from any machine holding the bundle *and* key access,
// not that key access travels with the bundle directory itself.
func TestUpSucceedsFromBundleCopiedToAnotherDirectory(t *testing.T) {
	original := testBundle(t)

	srcDir := filepath.Join(t.TempDir(), "machine-a", "bundle")
	if err := original.Save(srcDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	dstDir := filepath.Join(t.TempDir(), "machine-b", "elsewhere", "bundle")
	if err := copyDir(t, srcDir, dstDir); err != nil {
		t.Fatalf("copyDir: %v", err)
	}
	if err := os.RemoveAll(filepath.Dir(srcDir)); err != nil {
		t.Fatalf("remove original bundle tree: %v", err)
	}

	relocated, err := bundle.Load(dstDir)
	if err != nil {
		t.Fatalf("Load(%s): %v", dstDir, err)
	}

	host := newFakeHost()
	job := events.NewJob()
	issuer := &fakeCertIssuer{}

	err = Up(context.Background(), job, host, relocated, Options{RemoteDir: "/opt/farrier", CertIssuer: issuer})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	evs := drain(job)
	if len(evs) == 0 {
		t.Fatal("Up: emitted no events")
	}
	if last := evs[len(evs)-1]; last.State != events.StateSucceeded || last.Step != "" {
		t.Errorf("last event = %+v, want a job-terminal success", last)
	}
}

// copyDir physically copies src to dst, the same way a bundle directory
// moves to another machine: file bytes only, no directory-entry metadata
// that could carry a hidden reference back to src.
func copyDir(t *testing.T, src, dst string) error {
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
