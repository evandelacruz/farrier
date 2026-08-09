package drill

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/evandelacruz/farrier/internal/core/acme"
	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/restore"
	"github.com/evandelacruz/farrier/internal/core/state"

	_ "modernc.org/sqlite"
)

// The fixtures below mirror internal/core/promote's own (its fakes are
// unexported to that package): enough to drive a real Drill end to end
// against a real snapshot — one repository, a fake scratch host, and a
// keystore seeded with the certificate deploy.Up reuses.

const testDomain = "example.com"

const testTLSCert = `-----BEGIN CERTIFICATE-----
MIIBWzCCAQKgAwIBAgIBATAKBggqhkjOPQQDAjAWMRQwEgYDVQQDEwtleGFtcGxl
LmNvbTAgFw0yMDAxMDEwMDAwMDBaGA8yMDUwMDEwMTAwMDAwMFowFjEUMBIGA1UE
AxMLZXhhbXBsZS5jb20wWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAASF5HYWo+MY
90ad9Rn8thbSmiUUQOrAVZTOMUmJfIAfZLpzD7qRa8NxJr5V98mXcUIUTBMKt7DD
Mib5tq+uCbIqoz8wPTAOBgNVHQ8BAf8EBAMCB4AwEwYDVR0lBAwwCgYIKwYBBQUH
AwEwFgYDVR0RBA8wDYILZXhhbXBsZS5jb20wCgYIKoZIzj0EAwIDRwAwRAIgB/hE
Y+coSCnCy+Odz45V8kjIfCRHOOIjZfpJq3TwzK4CIBrtNGL+yx7XRaK9wLPwwPEU
IzD3Y+l63mLCbtCkW4ni
-----END CERTIFICATE-----
`

const testTLSKey = `-----BEGIN EC PRIVATE KEY-----
MHcCAQEEIFvcZ5AkAW+QtYYRmSwSM1W3wG8bn414Ine4za70ewlGoAoGCCqGSM49
AwEHoUQDQgAEheR2FqPjGPdGnfUZ/LYW0polFEDqwFWUzjFJiXyAH2S6cw+6kWvD
cSa+VffJl3FCFEwTCrewwzIm+bavrgmyKg==
-----END EC PRIVATE KEY-----
`

// forgeActionStatusRunning mirrors forge/reconcile.go's own unexported
// actionStatusRunning value (Forgejo Actions' status enum) — this package
// can't import it, so the fixed integer is reproduced here, the same way
// internal/core/promote's own promote_test.go does.
const forgeActionStatusRunning = 6

func testKeyValues() map[string]string {
	names := []string{
		forge.KeySecretKey, forge.KeyInternalToken, forge.KeyLFSJWTSecret,
		state.KeySSHHostKey, state.KeySSHHostKeyPublic,
	}
	values := map[string]string{}
	for _, name := range names {
		values[name] = "value-for-" + name
	}
	// The runner secret carries Forgejo's registration format rather than
	// a "value-for-..." placeholder (FORGE-005, forge.ValidateRunnerSecret).
	values[forge.KeyRunnerSecret] = "0123456789abcdef0123456789abcdef01234567"
	values[state.KeyTLSCertificate] = testTLSCert
	values[state.KeyTLSPrivateKey] = testTLSKey
	return values
}

type fakeKeyExporter struct {
	values map[string]string
}

func (f *fakeKeyExporter) Names() []string {
	names := make([]string, 0, len(f.values))
	for name := range f.values {
		names = append(names, name)
	}
	return names
}

func (f *fakeKeyExporter) Resolve(ctx context.Context, name string) (keystore.Secret, error) {
	return keystore.NewSecret(f.values[name]), nil
}

type fakeGitExporter struct {
	remotes []state.Remote
}

func (f *fakeGitExporter) Remotes(ctx context.Context) ([]state.Remote, error) {
	return f.remotes, nil
}

type fakeGitCapturer struct{}

func (fakeGitCapturer) Archive(ctx context.Context, remote state.Remote) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("objects-bytes-" + remote.Name)), nil
}

func (fakeGitCapturer) Refs(ctx context.Context, remote state.Remote) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("refs-bytes-" + remote.Name)), nil
}

type fakeDatabaseExporter struct {
	data []byte
}

func (f *fakeDatabaseExporter) Snapshot(ctx context.Context) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

// newTestDatabaseBytes builds a real, queryable SQLite file with the tables
// backup.Verify's cross-consistency check reads plus the two Forgejo
// Actions tables forge.ReconcileCI would touch, seeded with one repository
// and one orphaned running run/job. The orphaned rows are what
// TestDrillLeavesOrphanedCIJobsAlone asserts drill does not reset.
func newTestDatabaseBytes(t *testing.T) []byte {
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
		`INSERT INTO repository (owner_name, lower_name) VALUES ('acme', 'widgets')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO action_run (id, status, updated) VALUES (1, ?, 1000)`, forgeActionStatusRunning); err != nil {
		t.Fatalf("seed action_run: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO action_run_job (id, run_id, status, updated) VALUES (1, 1, ?, 1000)`, forgeActionStatusRunning); err != nil {
		t.Fatalf("seed action_run_job: %v", err)
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

func mustLocalBlob(t *testing.T, dir string) blob.Adapter {
	t.Helper()
	a, err := blob.NewLocal(dir)
	if err != nil {
		t.Fatalf("blob.NewLocal: %v", err)
	}
	return a
}

// buildSnapshotIn runs a real backup.Backup into destDir and returns the
// key it was written under.
func buildSnapshotIn(t *testing.T, destDir string, identity *age.X25519Identity) string {
	t.Helper()
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
	key, err := backup.Backup(context.Background(), events.NewJob(), opts)
	if err != nil {
		t.Fatalf("build snapshot: Backup: %v", err)
	}
	return key
}

// buildSnapshot writes one real snapshot to a fresh destination and returns
// the source adapter, the identity that decrypts it, its key, and the
// destination directory behind the adapter.
func buildSnapshot(t *testing.T) (blob.Adapter, *age.X25519Identity, string, string) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	destDir := t.TempDir()
	key := buildSnapshotIn(t, destDir, identity)

	source, err := blob.NewLocal(destDir)
	if err != nil {
		t.Fatalf("open snapshot source: %v", err)
	}
	return source, identity, key, destDir
}

type fakeCertIssuer struct{}

func (fakeCertIssuer) EnsureValid(cfg acme.Config, existing *acme.Certificate, now time.Time) (*acme.Certificate, bool, error) {
	if existing == nil {
		return nil, false, errors.New("fakeCertIssuer: no persisted certificate to reuse")
	}
	return existing, false, nil
}

func testBundle(t *testing.T, keysDir string) *bundle.Bundle {
	t.Helper()
	return &bundle.Bundle{
		Manifest: *bundle.NewManifest(testDomain, map[string]string{
			"forgejo": "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("a", 64),
			"caddy":   "docker.io/library/caddy@sha256:" + strings.Repeat("b", 64),
		}, bundle.DriverConfig{
			Keystore: bundle.DriverRef{Driver: "file", Config: map[string]any{"path": keysDir}},
			Blob:     bundle.DriverRef{Driver: "local", Config: map[string]any{"path": t.TempDir()}},
		}, bundle.ACMEConfig{DNSProvider: "manual", Email: "ops@example.com"}),
		Compose: map[string][]byte{
			"docker-compose.yml": []byte("services:\n  forgejo:\n    image: x\n  caddy:\n    image: y\n"),
		},
	}
}

// fakeHost implements Host (restore.Host: deploy.Host + RunStdin) without a
// real SSH connection. failOn, when set, fails any command containing it —
// how the failing-step tests drive a failure at a chosen step of the boot.
type fakeHost struct {
	mu       sync.Mutex
	files    map[string]string
	commands []string
	failOn   string
}

func newFakeHost() *fakeHost {
	return &fakeHost{files: make(map[string]string)}
}

func (f *fakeHost) record(command string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	if f.failOn != "" && strings.Contains(command, f.failOn) {
		return errors.New("fakeHost: command failed: " + command)
	}
	return nil
}

func (f *fakeHost) Output(ctx context.Context, command string) ([]byte, error) {
	if err := f.record(command); err != nil {
		return nil, err
	}
	// deploy.ReadStateVersion's read of the recorded forge version: serve it
	// out of the same map WriteFile stores into, so the fake's reads and
	// writes agree the way a real host's do (UPGR-003).
	if rest, ok := strings.CutPrefix(command, "if [ -f '"); ok {
		f.mu.Lock()
		defer f.mu.Unlock()
		p, _, _ := strings.Cut(rest, "'")
		return []byte(f.files[p]), nil
	}
	return nil, nil
}

func (f *fakeHost) Close() error { return nil }

func (f *fakeHost) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = string(content)
	return nil
}

func (f *fakeHost) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	return f.record(command)
}

func (f *fakeHost) CheckHost(ctx context.Context) error {
	return f.record("docker version")
}

func (f *fakeHost) RunStdin(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	if _, err := io.Copy(io.Discard, stdin); err != nil {
		return err
	}
	return f.record(command)
}

func (f *fakeHost) commandsContaining(substr string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.commands {
		if strings.Contains(c, substr) {
			out = append(out, c)
		}
	}
	return out
}

// fixture is a ready-to-run drill: an Options that, unmodified, completes
// successfully, plus the key of the snapshot it will drill and the
// destination directory that snapshot lives in.
type fixture struct {
	opts        Options
	snapshotKey string
	destDir     string
}

func (f fixture) host() *fakeHost { return f.opts.Host.(*fakeHost) }

// newFixture builds a fixture whose Options runs a complete drill
// successfully.
func newFixture(t *testing.T) fixture {
	t.Helper()
	source, identity, key, destDir := buildSnapshot(t)
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

	return fixture{
		opts: Options{
			RemoteDir:  "/opt/farrier-drill",
			WorkDir:    filepath.Join(t.TempDir(), "work"),
			Bundle:     testBundle(t, keysDir),
			Source:     source,
			Identity:   identity,
			Keystore:   keystoreDriver,
			Blobs:      mustLocalBlob(t, t.TempDir()),
			Host:       newFakeHost(),
			CertIssuer: fakeCertIssuer{},
		},
		snapshotKey: key,
		destDir:     destDir,
	}
}

// stepOutcomes indexes a job's step events by step name.
func stepOutcomes(job *events.Job) (started, terminal map[string]int) {
	started, terminal = map[string]int{}, map[string]int{}
	for _, ev := range job.Events() {
		if ev.Step == "" {
			continue
		}
		switch ev.State {
		case events.StateStarted:
			started[ev.Step]++
		case events.StateSucceeded, events.StateFailed:
			terminal[ev.Step]++
		}
	}
	return started, terminal
}

// TestDrillEndToEnd is DRIL-001's happy path: the most recent backup is
// restored onto the scratch target, the full stack boots there, and the
// drill reports success.
func TestDrillEndToEnd(t *testing.T) {
	f := newFixture(t)
	wantKey, host := f.snapshotKey, f.host()

	job := events.NewJob()
	report, err := Drill(context.Background(), job, f.opts)
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	if !job.Done() {
		t.Fatal("Drill did not end the job")
	}
	if !report.Succeeded() {
		t.Fatalf("report.Succeeded() = false, failure = %+v", report.Failure)
	}
	if report.SnapshotKey != wantKey {
		t.Errorf("report.SnapshotKey = %q, want %q", report.SnapshotKey, wantKey)
	}
	if report.SnapshotAge < 0 {
		t.Errorf("report.SnapshotAge = %v, want a non-negative age", report.SnapshotAge)
	}

	// The full stack booted: deploy.Up converged the scratch target.
	if len(host.commandsContaining("docker compose up -d --remove-orphans")) != 1 {
		t.Errorf("want exactly one converge command, got %v", host.commands)
	}
	// The snapshot's database reached the scratch target under the drill's
	// own remote directory, not production's.
	if len(host.commandsContaining("cat > '/opt/farrier-drill/state/gitea/gitea.db'")) != 1 {
		t.Errorf("want exactly one database placement command, got %v", host.commands)
	}

	// Drill's own step plus every relayed restore and deploy step reached
	// exactly one terminal event on the drill's job (CORE-002).
	wantSteps := []string{
		StepResolveSnapshot,
		restore.StepFetch, restore.StepDecrypt, restore.StepVerify,
		restore.StepInstallKeys, restore.StepPlaceState, restore.StepRestoreBlobs,
		deploy.StepCheckHost, deploy.StepConfigureForge, deploy.StepConfigureTLS,
		deploy.StepConfigureState, deploy.StepConfigureSSHKey, deploy.StepConverge,
		deploy.StepWaitForge, deploy.StepWaitCaddy,
	}
	started, terminal := stepOutcomes(job)
	for _, step := range wantSteps {
		if started[step] != 1 {
			t.Errorf("step %s started %d time(s), want 1", step, started[step])
		}
		if terminal[step] != 1 {
			t.Errorf("step %s reached a terminal event %d time(s), want 1", step, terminal[step])
		}
	}
}

// TestDrillDrillsTheMostRecentSnapshot pins DRIL-001's "the most recent
// backup": with more than one object at the destination, the newest is the
// one drilled and the one the report names.
func TestDrillDrillsTheMostRecentSnapshot(t *testing.T) {
	f := newFixture(t)
	newestKey := f.snapshotKey

	// An older object at the same destination. It is never fetched, so its
	// contents don't matter — only that it sorts older by modified time.
	stale := filepath.Join(f.destDir, "snapshot-stale.age")
	if err := os.WriteFile(stale, []byte("not a real snapshot"), 0o600); err != nil {
		t.Fatalf("seed stale object: %v", err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("age stale object: %v", err)
	}

	job := events.NewJob()
	report, err := Drill(context.Background(), job, f.opts)
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	if report.SnapshotKey != newestKey {
		t.Fatalf("report.SnapshotKey = %q, want the newest snapshot %q", report.SnapshotKey, newestKey)
	}

	// The fetch step names the same key: drill resolved once and handed
	// that exact key to restore rather than letting it re-resolve.
	var fetched string
	for _, ev := range job.Events() {
		if ev.Step == restore.StepFetch && ev.State == events.StateSucceeded {
			fetched = ev.Detail
		}
	}
	if !strings.Contains(fetched, newestKey) {
		t.Errorf("fetch detail = %q, want it to name %q", fetched, newestKey)
	}
}

// TestDrillReportsTheSpecificFailingStep is DRIL-001's "report ... the
// specific failing step": every failure names the step it happened at, in
// the Report, in the returned error, and on the job's terminal event.
func TestDrillReportsTheSpecificFailingStep(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(t *testing.T, opts *Options)
		wantStep string
	}{
		{
			name: "no snapshots at the destination",
			mutate: func(t *testing.T, opts *Options) {
				opts.Source = mustLocalBlob(t, t.TempDir())
			},
			wantStep: StepResolveSnapshot,
		},
		{
			name: "snapshot cannot be decrypted",
			mutate: func(t *testing.T, opts *Options) {
				other, err := age.GenerateX25519Identity()
				if err != nil {
					t.Fatalf("generate identity: %v", err)
				}
				opts.Identity = other
			},
			wantStep: restore.StepDecrypt,
		},
		{
			name: "the stack does not converge",
			mutate: func(t *testing.T, opts *Options) {
				opts.Host.(*fakeHost).failOn = "docker compose up -d --remove-orphans"
			},
			wantStep: deploy.StepConverge,
		},
		{
			name: "the host is not reachable",
			mutate: func(t *testing.T, opts *Options) {
				opts.Host.(*fakeHost).failOn = "docker version"
			},
			wantStep: deploy.StepCheckHost,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := newFixture(t).opts
			tt.mutate(t, &opts)

			job := events.NewJob()
			report, err := Drill(context.Background(), job, opts)
			if err == nil {
				t.Fatal("Drill: want an error, got nil")
			}
			if report.Succeeded() {
				t.Fatal("report.Succeeded() = true, want false")
			}
			if report.Failure.Step != tt.wantStep {
				t.Errorf("report.Failure.Step = %q, want %q", report.Failure.Step, tt.wantStep)
			}

			// The same step is reachable from the error itself, so a caller
			// can act on it without holding the Report.
			var failure *Failure
			if !errors.As(err, &failure) {
				t.Fatalf("Drill error = %T, want a *Failure", err)
			}
			if failure.Step != tt.wantStep {
				t.Errorf("error step = %q, want %q", failure.Step, tt.wantStep)
			}

			// And it reaches both frontends: the job's terminal event names
			// the step too (CORE-002, XCUT-002).
			emitted := job.Events()
			last := emitted[len(emitted)-1]
			if last.Step != "" {
				t.Fatalf("last event step = %q, want the job-terminal event", last.Step)
			}
			if !strings.Contains(last.Detail, tt.wantStep) {
				t.Errorf("terminal event detail = %q, want it to name step %q", last.Detail, tt.wantStep)
			}
		})
	}
}

// TestDrillLeavesOrphanedCIJobsAlone pins the decision recorded in the
// package doc comment: unlike promote (FAIL-003), a drill never reconciles
// CI. Re-queueing production's orphaned `running` jobs would arm the drill
// instance — which carries production's identity — to run production's CI
// for real.
func TestDrillLeavesOrphanedCIJobsAlone(t *testing.T) {
	job := events.NewJob()
	if _, err := Drill(context.Background(), job, newFixture(t).opts); err != nil {
		t.Fatalf("Drill: %v", err)
	}

	for _, ev := range job.Events() {
		if ev.Step == forge.StepCIReconcile {
			t.Fatalf("drill emitted a %s event (%q); a drill must not reconcile CI", forge.StepCIReconcile, ev.Detail)
		}
	}
}

// TestDrillNeverTouchesDNS pins DRIL-001's hardest boundary structurally
// rather than behaviorally: flipping the bundle's DNS record is promote's
// job, and a drill that repointed the domain at a scratch host would take
// production down. Options exposes no DNS field, and this asserts the
// package cannot reach a driver another way either.
func TestDrillNeverTouchesDNS(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	for name, pkg := range pkgs {
		for path, file := range pkg.Files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			for _, imp := range file.Imports {
				if strings.Contains(imp.Path.Value, "internal/core/dns") {
					t.Errorf("%s (package %s) imports %s; drill must never touch DNS", path, name, imp.Path.Value)
				}
			}
		}
	}
}

func TestOptionsValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{"missing work dir", func(o *Options) { o.WorkDir = "" }},
		{"missing remote dir", func(o *Options) { o.RemoteDir = "" }},
		{"missing bundle", func(o *Options) { o.Bundle = nil }},
		{"missing source", func(o *Options) { o.Source = nil }},
		{"missing identity", func(o *Options) { o.Identity = nil }},
		{"missing keystore", func(o *Options) { o.Keystore = nil }},
		{"missing blobs", func(o *Options) { o.Blobs = nil }},
		{"missing host", func(o *Options) { o.Host = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := newFixture(t).opts
			tt.mutate(&opts)

			job := events.NewJob()
			report, err := Drill(context.Background(), job, opts)
			if err == nil {
				t.Fatalf("Drill: want error for %s, got nil", tt.name)
			}
			// A failure before any step ran carries no step, and says so
			// rather than blaming one.
			if report.Failure == nil || report.Failure.Step != "" {
				t.Fatalf("report.Failure = %+v, want a step-less failure", report.Failure)
			}
			if !strings.Contains(err.Error(), "before any step") {
				t.Errorf("error = %q, want it to say the drill failed before any step", err)
			}
		})
	}
}
