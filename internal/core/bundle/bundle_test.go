package bundle

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func validManifest() *Manifest {
	return NewManifest(
		"forge.example.com",
		map[string]string{
			"forgejo": "codeberg.org/forgejo/forgejo@sha256:" + fakeDigest,
		},
		DriverConfig{
			Keystore: DriverRef{Driver: "file", Config: map[string]any{"path": "/keys/bundle.key"}},
			Blob:     DriverRef{Driver: "local", Config: map[string]any{"path": "/data/blobs"}},
		},
		ACMEConfig{DNSProvider: "manual"},
	)
}

const fakeDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func validBundle() *Bundle {
	return &Bundle{
		Manifest: *validManifest(),
		Compose:  map[string][]byte{"docker-compose.yml": []byte("services: {}\n")},
	}
}

func TestManifestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr bool
	}{
		{"valid", func(m *Manifest) {}, false},
		{"missing domain", func(m *Manifest) { m.Domain = "" }, true},
		{"no images", func(m *Manifest) { m.Images = nil }, true},
		{"tag-pinned image", func(m *Manifest) { m.Images["forgejo"] = "forgejo/forgejo:11" }, true},
		{"missing keystore driver", func(m *Manifest) { m.Drivers.Keystore.Driver = "" }, true},
		{"missing blob driver", func(m *Manifest) { m.Drivers.Blob.Driver = "" }, true},
		{"missing acme dns provider", func(m *Manifest) { m.ACME.DNSProvider = "" }, true},
		{"missing state kind", func(m *Manifest) { m.State = m.State[1:] }, true},
		{"duplicate state kind", func(m *Manifest) { m.State = append(m.State, m.State[0]) }, true},
		{"missing checksum algorithm", func(m *Manifest) { m.ChecksumAlgorithm = "" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest()
			tc.mutate(m)
			err := m.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestNewManifestDeclaresAllFourStateKinds(t *testing.T) {
	m := validManifest()
	if len(m.State) != len(AllStateKinds) {
		t.Fatalf("got %d state declarations, want %d", len(m.State), len(AllStateKinds))
	}
	for _, kind := range AllStateKinds {
		found := false
		for _, d := range m.State {
			if d.Kind == kind {
				found = true
			}
		}
		if !found {
			t.Fatalf("state kind %q not declared", kind)
		}
	}
}

func TestSaveRejectsInvalidManifest(t *testing.T) {
	b := validBundle()
	b.Manifest.Domain = ""
	if err := b.Save(t.TempDir()); err == nil {
		t.Fatal("Save() = nil, want error for invalid manifest")
	}
}

func TestSaveRejectsNoComposeFiles(t *testing.T) {
	b := validBundle()
	b.Compose = nil
	if err := b.Save(t.TempDir()); err == nil {
		t.Fatal("Save() = nil, want error for no Compose files")
	}
}

func TestLoadRejectsMissingCompose(t *testing.T) {
	dir := t.TempDir()
	raw, err := yaml.Marshal(&validBundle().Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load() = nil, want error for missing compose/ directory")
	}
}

func TestLoadRejectsEmptyComposeDir(t *testing.T) {
	dir := t.TempDir()
	raw, err := yaml.Marshal(&validBundle().Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ComposeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load() = nil, want error for empty compose/ directory")
	}
}

// TestSaveRemovesStaleComposeFiles proves a re-save doesn't leave behind
// compose files from a previous save that are no longer in b.Compose — the
// case that matters for upgrade, which re-renders compose files on every
// pinned-digest bump.
func TestSaveRemovesStaleComposeFiles(t *testing.T) {
	dir := t.TempDir()
	b := validBundle()
	b.Compose["old.yml"] = []byte("services:\n  old: {}\n")
	if err := b.Save(dir); err != nil {
		t.Fatalf("Save() = %v", err)
	}

	b.Compose = map[string][]byte{"docker-compose.yml": []byte("services: {}\n")}
	if err := b.Save(dir); err != nil {
		t.Fatalf("Save() = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ComposeDir, "old.yml")); !os.IsNotExist(err) {
		t.Fatalf("old.yml still present after re-save, err = %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if !reflect.DeepEqual(loaded.Compose, b.Compose) {
		t.Fatalf("loaded compose = %+v, want %+v", loaded.Compose, b.Compose)
	}
}

// TestSaveLeavesNoStagingDir proves the compose/ staging directory used to
// make the swap-in atomic doesn't leak into the bundle directory.
func TestSaveLeavesNoStagingDir(t *testing.T) {
	dir := t.TempDir()
	if err := validBundle().Save(dir); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ComposeDir+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("staging dir still present after Save, err = %v", err)
	}
}

// TestPortability proves the CORE-001 requirement directly: a bundle saved
// to one directory, then physically copied byte-for-byte to a wholly
// different directory (standing in for "another machine"), loads back to an
// identical in-memory bundle. Nothing about Bundle or Manifest depends on
// the directory it was first written to.
func TestPortability(t *testing.T) {
	original := validBundle()
	original.Compose["runner.yml"] = []byte("services:\n  runner: {}\n")

	srcDir := filepath.Join(t.TempDir(), "machine-a", "bundle")
	if err := original.Save(srcDir); err != nil {
		t.Fatalf("Save() = %v", err)
	}

	dstDir := filepath.Join(t.TempDir(), "machine-b", "elsewhere", "bundle")
	if err := copyDir(t, srcDir, dstDir); err != nil {
		t.Fatalf("copyDir() = %v", err)
	}

	loaded, err := Load(dstDir)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if !reflect.DeepEqual(loaded.Manifest, original.Manifest) {
		t.Fatalf("manifest changed after copy:\ngot  %+v\nwant %+v", loaded.Manifest, original.Manifest)
	}
	if !reflect.DeepEqual(loaded.Compose, original.Compose) {
		t.Fatalf("compose files changed after copy:\ngot  %+v\nwant %+v", loaded.Compose, original.Compose)
	}
}

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
