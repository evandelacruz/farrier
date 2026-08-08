package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"

	_ "modernc.org/sqlite"
)

// newTestDatabaseBytes builds a fresh SQLite file with the three tables
// Verify's cross-consistency check reads — repository (sized down to
// owner_name/lower_name, the columns verifyRepositoryReferences uses),
// lfs_meta_object, and attachment — inserts the given rows, and returns the
// file's raw bytes: exactly the shape a fakeDatabaseExporter.Snapshot
// stands in for state.DatabaseExporter returning a real captured database.
func newTestDatabaseBytes(t *testing.T, repos [][2]string, lfsOids, attachmentUUIDs []string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gitea.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	schema := []string{
		`CREATE TABLE repository (id INTEGER PRIMARY KEY, owner_name TEXT, lower_name TEXT)`,
		`CREATE TABLE lfs_meta_object (id INTEGER PRIMARY KEY, oid TEXT)`,
		`CREATE TABLE attachment (id INTEGER PRIMARY KEY, uuid TEXT)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	for _, r := range repos {
		if _, err := db.Exec(`INSERT INTO repository (owner_name, lower_name) VALUES (?, ?)`, r[0], r[1]); err != nil {
			t.Fatalf("insert repository %v: %v", r, err)
		}
	}
	for _, oid := range lfsOids {
		if _, err := db.Exec(`INSERT INTO lfs_meta_object (oid) VALUES (?)`, oid); err != nil {
			t.Fatalf("insert lfs_meta_object %q: %v", oid, err)
		}
	}
	for _, uuid := range attachmentUUIDs {
		if _, err := db.Exec(`INSERT INTO attachment (uuid) VALUES (?)`, uuid); err != nil {
			t.Fatalf("insert attachment %q: %v", uuid, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// defaultTestRepos matches validParams' fakeGitExporter remotes
// ("acme/widgets", "acme/gadgets"), so the default snapshot Run produces in
// backup_test.go passes cross-consistency out of the box.
var defaultTestRepos = [][2]string{{"acme", "widgets"}, {"acme", "gadgets"}}

func writeSnapshotFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("mkdir for %s: %v", full, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

// cleanManifest returns a manifest and a matching snapshot directory on
// disk that Verify passes cleanly: one database component (a real,
// queryable SQLite file whose repository table matches the git components),
// two components per repository (refs + objects), one key, and no blobs —
// the minimal shape every defect-injection test below starts from and then
// mutates exactly one thing about.
func cleanManifest(t *testing.T) (dir string, manifest *Manifest) {
	t.Helper()
	dir = t.TempDir()

	dbBytes := newTestDatabaseBytes(t, defaultTestRepos, nil, nil)
	writeSnapshotFile(t, dir, databaseFile, string(dbBytes))

	components := []Component{
		{Kind: bundle.StateKindDatabase, Name: databaseFile, Path: databaseFile},
	}
	for _, r := range defaultTestRepos {
		name := r[0] + "/" + r[1]
		refsPath := filepath.ToSlash(filepath.Join("repos", name+".refs.tar"))
		objectsPath := filepath.ToSlash(filepath.Join("repos", name+".tar"))
		writeSnapshotFile(t, dir, refsPath, "refs-"+name)
		writeSnapshotFile(t, dir, objectsPath, "objects-"+name)
		components = append(components,
			Component{Kind: bundle.StateKindGit, Name: name, Path: refsPath},
			Component{Kind: bundle.StateKindGit, Name: name, Path: objectsPath},
		)
	}
	writeSnapshotFile(t, dir, "keys/secret_key", "sk-value")
	components = append(components, Component{Kind: bundle.StateKindKeys, Name: "secret_key", Path: "keys/secret_key"})

	manifest = &Manifest{ChecksumAlgorithm: bundle.DefaultChecksumAlgorithm, Components: components}
	for i, c := range manifest.Components {
		checksum, err := checksumFile(filepath.Join(dir, filepath.FromSlash(c.Path)))
		if err != nil {
			t.Fatalf("checksum %s: %v", c.Path, err)
		}
		manifest.Components[i].Checksum = checksum
	}

	dbChecksum, err := checksumFile(filepath.Join(dir, databaseFile))
	if err != nil {
		t.Fatalf("checksum %s: %v", databaseFile, err)
	}
	for i, c := range manifest.Components {
		if c.Path == databaseFile {
			manifest.Components[i].Checksum = dbChecksum
		}
	}

	return dir, manifest
}

func TestVerifyPassesCleanSnapshot(t *testing.T) {
	dir, manifest := cleanManifest(t)
	if err := Verify(context.Background(), dir, manifest, []string{"secret_key"}); err != nil {
		t.Fatalf("Verify: %v, want nil", err)
	}
}

func TestVerifyDetectsChecksumMismatch(t *testing.T) {
	dir, manifest := cleanManifest(t)
	writeSnapshotFile(t, dir, "keys/secret_key", "tampered-value")

	err := Verify(context.Background(), dir, manifest, []string{"secret_key"})
	verr := requireVerifyError(t, err)
	requireDefect(t, verr, "checksum", "keys/secret_key")
}

func TestVerifyDetectsMissingComponentFile(t *testing.T) {
	dir, manifest := cleanManifest(t)
	if err := os.Remove(filepath.Join(dir, "keys", "secret_key")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	err := Verify(context.Background(), dir, manifest, []string{"secret_key"})
	verr := requireVerifyError(t, err)
	requireDefect(t, verr, "checksum", "keys/secret_key")
}

func TestVerifyDetectsMissingKeyMaterial(t *testing.T) {
	dir, manifest := cleanManifest(t)

	err := Verify(context.Background(), dir, manifest, []string{"secret_key", "internal_token"})
	verr := requireVerifyError(t, err)
	requireDefect(t, verr, "completeness", "internal_token")
}

func TestVerifyDetectsWrongDatabaseComponentCount(t *testing.T) {
	dir, manifest := cleanManifest(t)
	manifest.Components = append(manifest.Components, Component{
		Kind: bundle.StateKindDatabase, Name: "extra.sqlite", Path: databaseFile, Checksum: manifest.Components[0].Checksum,
	})

	err := Verify(context.Background(), dir, manifest, []string{"secret_key"})
	verr := requireVerifyError(t, err)
	requireDefect(t, verr, "completeness", "")
}

func TestVerifyDetectsUnknownChecksumAlgorithm(t *testing.T) {
	dir, manifest := cleanManifest(t)
	manifest.ChecksumAlgorithm = "md5"

	err := Verify(context.Background(), dir, manifest, []string{"secret_key"})
	verr := requireVerifyError(t, err)
	requireDefect(t, verr, "completeness", "")
}

func TestVerifyDetectsRepositoryWithNoCapturedGitComponent(t *testing.T) {
	dir := t.TempDir()
	repos := append([][2]string{}, defaultTestRepos...)
	repos = append(repos, [2]string{"acme", "orphan"})
	dbBytes := newTestDatabaseBytes(t, repos, nil, nil)
	writeSnapshotFile(t, dir, databaseFile, string(dbBytes))

	manifest := manifestForRepos(t, dir, defaultTestRepos)

	err := Verify(context.Background(), dir, manifest, nil)
	verr := requireVerifyError(t, err)
	requireDefect(t, verr, "cross-consistency", "acme/orphan")
}

func TestVerifyDetectsRepositoryMissingRefsComponent(t *testing.T) {
	dir, manifest := cleanManifest(t)

	// Drop the refs component for one repository, leaving its objects
	// component in place — the DB row should be flagged for the missing
	// half specifically, not just "not found".
	var filtered []Component
	for _, c := range manifest.Components {
		if c.Kind == bundle.StateKindGit && c.Name == "acme/widgets" && strings.HasSuffix(c.Path, ".refs.tar") {
			continue
		}
		filtered = append(filtered, c)
	}
	manifest.Components = filtered

	err := Verify(context.Background(), dir, manifest, nil)
	verr := requireVerifyError(t, err)
	requireDefect(t, verr, "cross-consistency", "acme/widgets")
}

func TestVerifyDetectsLFSReferenceWithNoCapturedBlob(t *testing.T) {
	dir := t.TempDir()
	dbBytes := newTestDatabaseBytes(t, defaultTestRepos, []string{"deadbeef"}, nil)
	writeSnapshotFile(t, dir, databaseFile, string(dbBytes))
	manifest := manifestForRepos(t, dir, defaultTestRepos)

	err := Verify(context.Background(), dir, manifest, nil)
	verr := requireVerifyError(t, err)
	requireDefect(t, verr, "cross-consistency", "lfs_meta_object.oid=deadbeef")
}

func TestVerifyDetectsAttachmentReferenceWithNoCapturedBlob(t *testing.T) {
	dir := t.TempDir()
	dbBytes := newTestDatabaseBytes(t, defaultTestRepos, nil, []string{"uuid-1234"})
	writeSnapshotFile(t, dir, databaseFile, string(dbBytes))
	manifest := manifestForRepos(t, dir, defaultTestRepos)

	err := Verify(context.Background(), dir, manifest, nil)
	verr := requireVerifyError(t, err)
	requireDefect(t, verr, "cross-consistency", "attachment.uuid=uuid-1234")
}

func TestVerifyPassesWhenLFSReferenceMatchesCapturedBlobKey(t *testing.T) {
	dir := t.TempDir()
	dbBytes := newTestDatabaseBytes(t, defaultTestRepos, []string{"deadbeef"}, nil)
	writeSnapshotFile(t, dir, databaseFile, string(dbBytes))
	manifest := manifestForRepos(t, dir, defaultTestRepos)

	writeSnapshotFile(t, dir, "blobs/lfs/de/ad/deadbeef", "lfs-object-bytes")
	checksum, err := checksumFile(filepath.Join(dir, "blobs/lfs/de/ad/deadbeef"))
	if err != nil {
		t.Fatalf("checksum: %v", err)
	}
	manifest.Components = append(manifest.Components, Component{
		Kind: bundle.StateKindBlobs, Name: "lfs/de/ad/deadbeef", Path: "blobs/lfs/de/ad/deadbeef", Checksum: checksum,
	})

	if err := Verify(context.Background(), dir, manifest, nil); err != nil {
		t.Fatalf("Verify: %v, want nil", err)
	}
}

func TestVerifyDetectsUnreadableDatabase(t *testing.T) {
	dir := t.TempDir()
	writeSnapshotFile(t, dir, databaseFile, "not a sqlite file")
	manifest := manifestForRepos(t, dir, nil)

	err := Verify(context.Background(), dir, manifest, nil)
	verr := requireVerifyError(t, err)
	requireDefect(t, verr, "cross-consistency", databaseFile)
}

func TestVerifyAggregatesMultipleDefects(t *testing.T) {
	dir, manifest := cleanManifest(t)
	writeSnapshotFile(t, dir, "keys/secret_key", "tampered-value")
	manifest.ChecksumAlgorithm = "md5"

	err := Verify(context.Background(), dir, manifest, []string{"secret_key", "internal_token"})
	verr := requireVerifyError(t, err)
	if len(verr.Defects) < 3 {
		t.Fatalf("Defects = %+v, want at least 3 (checksum, algorithm, missing key)", verr.Defects)
	}
	requireDefect(t, verr, "checksum", "keys/secret_key")
	requireDefect(t, verr, "completeness", "internal_token")
	requireDefect(t, verr, "completeness", "")
}

// manifestForRepos writes a database component for dir's already-written
// databaseFile and a git component (refs+objects) for every repo in repos,
// returning a manifest with correct checksums — used by tests that build
// their own database fixture instead of cleanManifest's default one.
func manifestForRepos(t *testing.T, dir string, repos [][2]string) *Manifest {
	t.Helper()
	components := []Component{{Kind: bundle.StateKindDatabase, Name: databaseFile, Path: databaseFile}}
	for _, r := range repos {
		name := r[0] + "/" + r[1]
		refsPath := filepath.ToSlash(filepath.Join("repos", name+".refs.tar"))
		objectsPath := filepath.ToSlash(filepath.Join("repos", name+".tar"))
		writeSnapshotFile(t, dir, refsPath, "refs-"+name)
		writeSnapshotFile(t, dir, objectsPath, "objects-"+name)
		components = append(components,
			Component{Kind: bundle.StateKindGit, Name: name, Path: refsPath},
			Component{Kind: bundle.StateKindGit, Name: name, Path: objectsPath},
		)
	}
	manifest := &Manifest{ChecksumAlgorithm: bundle.DefaultChecksumAlgorithm, Components: components}
	for i, c := range manifest.Components {
		checksum, err := checksumFile(filepath.Join(dir, filepath.FromSlash(c.Path)))
		if err != nil {
			t.Fatalf("checksum %s: %v", c.Path, err)
		}
		manifest.Components[i].Checksum = checksum
	}
	return manifest
}

func requireVerifyError(t *testing.T, err error) *VerifyError {
	t.Helper()
	if err == nil {
		t.Fatal("Verify: want error, got nil")
	}
	verr, ok := err.(*VerifyError)
	if !ok {
		t.Fatalf("Verify error type = %T, want *VerifyError", err)
	}
	return verr
}

func requireDefect(t *testing.T, verr *VerifyError, check, subject string) {
	t.Helper()
	for _, d := range verr.Defects {
		if d.Check == check && (subject == "" || d.Subject == subject) {
			return
		}
	}
	t.Errorf("Defects = %+v, want one with Check=%q Subject=%q", verr.Defects, check, subject)
}
