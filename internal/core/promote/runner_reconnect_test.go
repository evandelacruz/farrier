package promote

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/state"

	_ "modernc.org/sqlite"
)

// FAIL-005 proof, end to end through a real Promote: a remote runner's
// registration survives promotion onto a fresh standby host, and the
// config that governs the URL a runner dials (app.ini's DOMAIN/ROOT_URL/
// SSH_DOMAIN, forge.RenderAppINI) carries only the bundle's permanent
// domain — never opts.DNSValue, the standby host's own address. Together
// these are spec.md "Runners across relocation"'s "remote runners
// reconnect automatically" — reconcile_test.go's
// TestReconcileCILeavesRunnerRegistrationUntouched proves the database
// half at the unit level; this proves both halves survive a real
// promotion.

const testRunnerUUID = "11111111-2222-3333-4444-555555555555"
const testRunnerTokenHash = "deadbeefcafef00d"

// newTestDatabaseBytesWithRunner is newTestDatabaseBytes (promote_test.go)
// extended with Forgejo's action_runner table and one registered remote
// runner row — kept as its own fixture, rather than folded into the shared
// one, so this file's schema assumptions don't leak into every other
// promote test.
func newTestDatabaseBytesWithRunner(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gitea.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	stmts := []string{
		`CREATE TABLE repository (id INTEGER PRIMARY KEY, owner_name TEXT, lower_name TEXT)`,
		`CREATE TABLE lfs_meta_object (id INTEGER PRIMARY KEY, oid TEXT)`,
		`CREATE TABLE attachment (id INTEGER PRIMARY KEY, uuid TEXT)`,
		`CREATE TABLE action_run (id INTEGER PRIMARY KEY, status INTEGER, updated INTEGER)`,
		`CREATE TABLE action_run_job (id INTEGER PRIMARY KEY, run_id INTEGER, status INTEGER, updated INTEGER)`,
		`CREATE TABLE action_runner (id INTEGER PRIMARY KEY, uuid TEXT, name TEXT, token_hash TEXT, owner_id INTEGER)`,
		`INSERT INTO repository (owner_name, lower_name) VALUES ('acme', 'widgets')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	if _, err := db.Exec(
		`INSERT INTO action_runner (id, uuid, name, token_hash, owner_id) VALUES (1, ?, 'remote-1', ?, 0)`,
		testRunnerUUID, testRunnerTokenHash,
	); err != nil {
		t.Fatalf("seed action_runner: %v", err)
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

// buildSnapshotWithRunner is buildSnapshot (promote_test.go), reimplemented
// against newTestDatabaseBytesWithRunner's database instead — the
// relocated-bundle proof's own buildSnapshotForIdentity is the precedent
// for reimplementing buildSnapshot with one field swapped out.
func buildSnapshotWithRunner(t *testing.T) (blob.Adapter, *age.X25519Identity) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	destDir := t.TempDir()

	opts := backup.Options{
		WorkDir:        filepath.Join(t.TempDir(), "work"),
		ForgejoVersion: "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("c", 64),
		Destination:    destDir,
		Identity:       identity,
		Git:            &fakeGitExporter{remotes: []state.Remote{{Name: "acme/widgets"}}},
		GitCapturer:    fakeGitCapturer{},
		Database:       &fakeDatabaseExporter{data: newTestDatabaseBytesWithRunner(t)},
		Blobs:          mustLocalBlob(t, t.TempDir()),
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
	return source, identity
}

// stdinCapturingHost wraps fakeHost to also capture what RunStdin streamed
// in, keyed by the command it was streamed to. fakeHost's own RunStdin
// (promote_test.go) discards stdin — every other test only asserts which
// commands ran, never the bytes they carried — but this test needs the
// database restore.placeState actually placed, to inspect the registration
// row that landed on the standby host.
type stdinCapturingHost struct {
	*fakeHost
	mu    sync.Mutex
	stdin map[string][]byte
}

func newStdinCapturingHost() *stdinCapturingHost {
	return &stdinCapturingHost{fakeHost: newFakeHost(), stdin: map[string][]byte{}}
}

func (h *stdinCapturingHost) RunStdin(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.stdin[command] = data
	h.mu.Unlock()
	return h.fakeHost.RunStdin(ctx, command, bytes.NewReader(data), stdout, stderr)
}

// stdinFor returns the bytes streamed to the one RunStdin command
// containing substr, failing the test if there isn't exactly one match.
func (h *stdinCapturingHost) stdinFor(t *testing.T, substr string) []byte {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var matched []string
	for command := range h.stdin {
		if strings.Contains(command, substr) {
			matched = append(matched, command)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("commands containing %q: got %d, want 1 (%v)", substr, len(matched), matched)
	}
	return h.stdin[matched[0]]
}

func TestPromoteRemoteRunnerReconnectsWithoutReregistration(t *testing.T) {
	source, identity := buildSnapshotWithRunner(t)
	keysDir := t.TempDir()
	keystoreDriver, err := keystore.New("file", map[string]any{"path": keysDir})
	if err != nil {
		t.Fatalf("build keystore driver: %v", err)
	}
	values := testKeyValues()
	for _, name := range []string{state.KeyTLSCertificate, state.KeyTLSPrivateKey} {
		if err := os.WriteFile(filepath.Join(keysDir, name), []byte(values[name]), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	host := newStdinCapturingHost()
	opts := Options{
		RemoteDir:  "/opt/farrier",
		WorkDir:    filepath.Join(t.TempDir(), "work"),
		Bundle:     testBundle(t, keysDir),
		Source:     source,
		Identity:   identity,
		Keystore:   keystoreDriver,
		Blobs:      newFakeBlobTarget(),
		Host:       host,
		CertIssuer: fakeCertIssuer{},
		DNS:        &fakeDNSDriver{},
		DNSValue:   "203.0.113.10",
	}

	job := events.NewJob()
	if err := Promote(context.Background(), job, opts); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	// The registration Forgejo needs to recognize the remote runner landed
	// on the standby host unchanged: restore.placeState streams the whole
	// database file across, and nothing on that path ever queries
	// action_runner (reconcile_test.go proves ReconcileCI's own share of
	// that at the unit level).
	dbContent := host.stdinFor(t, "/opt/farrier/state/gitea/gitea.db")
	dbPath := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(dbPath, dbContent, 0o600); err != nil {
		t.Fatalf("write restored db: %v", err)
	}
	restoredDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer restoredDB.Close()

	var gotUUID, gotTokenHash string
	if err := restoredDB.QueryRow(`SELECT uuid, token_hash FROM action_runner WHERE id = 1`).Scan(&gotUUID, &gotTokenHash); err != nil {
		t.Fatalf("query restored action_runner: %v", err)
	}
	if gotUUID != testRunnerUUID || gotTokenHash != testRunnerTokenHash {
		t.Errorf("restored action_runner = (uuid=%q, token_hash=%q), want unchanged (uuid=%q, token_hash=%q)",
			gotUUID, gotTokenHash, testRunnerUUID, testRunnerTokenHash)
	}

	// The config a runner dials — app.ini's DOMAIN/ROOT_URL/SSH_DOMAIN —
	// carries only the bundle's permanent domain, never the standby host's
	// own address opts.DNSValue names. A remote runner already pointed at
	// the domain needs no reconfiguration once DNS-004's 60-second TTL
	// catches up.
	appINI, ok := host.files["/opt/farrier/forge/app.ini"]
	if !ok {
		t.Fatal("no app.ini was shipped to the standby host")
	}
	if !strings.Contains(appINI, "DOMAIN = "+testDomain) {
		t.Errorf("app.ini does not carry the bundle domain %q:\n%s", testDomain, appINI)
	}
	if strings.Contains(appINI, opts.DNSValue) {
		t.Errorf("app.ini contains the standby host's own address %q — a runner configured against it would be stranded on the next promotion", opts.DNSValue)
	}
}
