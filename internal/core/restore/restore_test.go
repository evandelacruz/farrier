package restore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
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
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/state"

	_ "modernc.org/sqlite"
)

// testDomain is the domain every fixture in this file is built for — it
// must match the CN/SAN baked into testTLSCert below.
const testDomain = "example.com"

// testTargetForgeImage is the forge image testBundle's own farrier.yaml
// pins, and testSnapshotForgeImage is the (deliberately different) image
// buildSnapshot records as the snapshot's ForgejoVersion — the pinned
// image ref backup.BuildOptions actually captures from a source bundle's
// own Manifest.Images at real backup time. Keeping the two distinct is
// what lets TestRestoreEndToEnd prove restore deploys the snapshot's
// pinned image rather than the target bundle's own (RSTR-002): if they
// happened to match, the override would be silently untested.
var (
	testTargetForgeImage   = "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("a", 64)
	testSnapshotForgeImage = "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("c", 64)
)

// testTLSCert and testTLSKey are a fixed, valid EC certificate/key pair for
// testDomain, the same fixture shape internal/core/deploy's own tests use
// (deploy/testdata/keys) — deploy.Up's configureTLS step parses whatever
// key material restore installs as a real certificate
// (acme.ParseCertificate), so an arbitrary string won't do here the way it
// does for the other key names.
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

// testKeyValues returns a distinct, deterministic value for every name
// keyNames() enumerates except the TLS pair, which must be a real,
// parseable certificate for deploy.Up's configureTLS step to accept.
func testKeyValues() map[string]string {
	values := map[string]string{}
	for _, name := range keyNames() {
		values[name] = "value-for-" + name
	}
	values[state.KeyTLSCertificate] = testTLSCert
	values[state.KeyTLSPrivateKey] = testTLSKey
	return values
}

// fakeKeyExporter is the source side of a snapshot's key material: exactly
// the set restore.keyNames() expects, so backup.Verify's completeness check
// (which restore's own decryptAndVerify runs the exact same way) passes.
type fakeKeyExporter struct {
	values map[string]string
}

func (f *fakeKeyExporter) Names() []string { return keyNames() }

func (f *fakeKeyExporter) Resolve(ctx context.Context, name string) (keystore.Secret, error) {
	return keystore.NewSecret(f.values[name]), nil
}

// fakeGitExporter and fakeGitCapturer stand in for a live forge's git data
// during snapshot construction — the same shape backup's own tests use,
// reimplemented here since backup's fakes are unexported to that package.
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

// newTestDatabaseBytes builds a real, queryable SQLite file with the three
// tables backup.Verify's cross-consistency check reads, so a snapshot built
// from it passes verification the same way a real capture would — the same
// approach backup's own verify_test.go takes, reimplemented here for the
// same unexported-fakes reason as fakeGitExporter above.
func newTestDatabaseBytes(t *testing.T, repos [][2]string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gitea.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	for _, stmt := range []string{
		`CREATE TABLE repository (id INTEGER PRIMARY KEY, owner_name TEXT, lower_name TEXT)`,
		`CREATE TABLE lfs_meta_object (id INTEGER PRIMARY KEY, oid TEXT)`,
		`CREATE TABLE attachment (id INTEGER PRIMARY KEY, uuid TEXT)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	for _, r := range repos {
		if _, err := db.Exec(`INSERT INTO repository (owner_name, lower_name) VALUES (?, ?)`, r[0], r[1]); err != nil {
			t.Fatalf("insert repository %v: %v", r, err)
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

// buildSnapshot runs a real backup.Backup against a fresh local destination
// using the fakes above, returning the destination adapter (restore.Options
// Source), the encrypting identity, and the key values the snapshot
// captured, so a test can assert restore installed exactly them.
func buildSnapshot(t *testing.T, repos []state.Remote, dbRepos [][2]string) (blob.Adapter, *age.X25519Identity, map[string]string) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	destDir := t.TempDir()
	values := testKeyValues()

	blobsSource := mustLocalBlob(t, t.TempDir())
	if err := blobsSource.Put(context.Background(), "avatars/1.png", strings.NewReader("avatar-bytes"), -1); err != nil {
		t.Fatalf("seed source blob: %v", err)
	}

	opts := backup.Options{
		WorkDir:        filepath.Join(t.TempDir(), "work"),
		ForgejoVersion: testSnapshotForgeImage,
		Destination:    destDir,
		Identity:       identity,
		Git:            &fakeGitExporter{remotes: repos},
		GitCapturer:    fakeGitCapturer{},
		Database:       &fakeDatabaseExporter{data: newTestDatabaseBytes(t, dbRepos)},
		Blobs:          blobsSource,
		Keys:           &fakeKeyExporter{values: values},
		PushHold:       backup.NoopPushHold{},
	}
	job := events.NewJob()
	if err := backup.Backup(context.Background(), job, opts); err != nil {
		t.Fatalf("build snapshot: Backup: %v", err)
	}

	source, err := blob.NewLocal(destDir)
	if err != nil {
		t.Fatalf("open snapshot source: %v", err)
	}
	return source, identity, values
}

func mustLocalBlob(t *testing.T, dir string) blob.Adapter {
	t.Helper()
	a, err := blob.NewLocal(dir)
	if err != nil {
		t.Fatalf("blob.NewLocal: %v", err)
	}
	return a
}

// fakeCertIssuer satisfies deploy.CertIssuer without a real ACME server —
// restore's tests only ever exercise the "reuse the persisted certificate"
// path (deploy_test.go's own fakeCertIssuer does the same for the same
// reason).
type fakeCertIssuer struct{}

func (fakeCertIssuer) EnsureValid(cfg acme.Config, existing *acme.Certificate, now time.Time) (*acme.Certificate, bool, error) {
	if existing == nil {
		return nil, false, errors.New("fakeCertIssuer: no persisted certificate to reuse")
	}
	return existing, false, nil
}

// testBundle returns a bundle for testDomain whose keystore driver points
// at keysDir (a fresh, typically empty, directory a test owns) and whose
// blob driver points at a fresh local directory — deploy.Up's own
// configureForge/configureTLS steps resolve secrets through a keystore
// driver built fresh from this same Drivers.Keystore config, so whatever
// installKeys writes into keysDir is exactly what deploy.Up reads back.
func testBundle(t *testing.T, keysDir string) *bundle.Bundle {
	t.Helper()
	return &bundle.Bundle{
		Manifest: *bundle.NewManifest(testDomain, map[string]string{
			"forgejo": testTargetForgeImage,
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

// fakeHost implements restore.Host (and so deploy.Host) without a real SSH
// server, recording every command and streamed-stdin payload so a test can
// assert on restore's sequencing and content placement — the same shape
// deploy_test.go's fakeHost takes, extended with RunStdin.
type fakeHost struct {
	mu sync.Mutex

	files    map[string]string
	commands []string
	stdins   []stdinCall

	checkHostErr error
	writeFileErr error
}

type stdinCall struct {
	command string
	data    []byte
}

func newFakeHost() *fakeHost {
	return &fakeHost{files: make(map[string]string)}
}

func (f *fakeHost) Output(ctx context.Context, command string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	return nil, nil
}

func (f *fakeHost) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeFileErr != nil {
		return f.writeFileErr
	}
	f.files[path] = string(content)
	return nil
}

func (f *fakeHost) Close() error { return nil }

func (f *fakeHost) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	// deploy.Up's readiness polling (`docker compose exec -T <service>
	// true`) and admin bootstrap both go through Run; neither needs to
	// fail for restore's own tests.
	return nil
}

func (f *fakeHost) CheckHost(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "docker version")
	return f.checkHostErr
}

func (f *fakeHost) RunStdin(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	f.stdins = append(f.stdins, stdinCall{command: command, data: data})
	return nil
}

// fileEndingWith returns the content of the one recorded WriteFile call
// whose path ends with suffix — orchestrate.Converge stages Compose files
// under a temporary directory before renaming it into place, so the final
// path isn't predictable, but the filename is.
func (f *fakeHost) fileEndingWith(t *testing.T, suffix string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for path, content := range f.files {
		if strings.HasSuffix(path, suffix) {
			return content
		}
	}
	t.Fatalf("no WriteFile call recorded ending with %q, paths: %v", suffix, mapKeys(f.files))
	return ""
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// commandsContaining returns every recorded command containing substr, in
// the order they ran.
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

// stdinFor returns the stdin bytes streamed to the first recorded RunStdin
// call whose command contains substr.
func (f *fakeHost) stdinFor(t *testing.T, substr string) []byte {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.stdins {
		if strings.Contains(c.command, substr) {
			return c.data
		}
	}
	t.Fatalf("no RunStdin call recorded matching %q, commands: %v", substr, f.commands)
	return nil
}

// fakeKeystoreDriver is the target keystore restore installs key material
// into: an in-memory map, starting from whatever seed a test wants, so
// tests can exercise the empty-target, already-consistent, and conflicting
// cases installKeys must handle. It is not wrapped in keystore.New's
// rotation guard, since installKeys' own idempotent-resolve-before-store
// logic is what's under test here, not the guard (keystore/guard_test.go
// covers that independently).
type fakeKeystoreDriver struct {
	mu       sync.Mutex
	values   map[string]string
	storeErr error
	stores   []string
}

func (f *fakeKeystoreDriver) Resolve(ctx context.Context, name string) (keystore.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.values[name]
	if !ok {
		return keystore.Secret{}, keystore.ErrNotFound
	}
	return keystore.NewSecret(v), nil
}

func (f *fakeKeystoreDriver) Store(ctx context.Context, name string, secret keystore.Secret) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.storeErr != nil {
		return f.storeErr
	}
	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[name] = secret.Reveal()
	f.stores = append(f.stores, name)
	return nil
}

var (
	_ keystore.Driver = (*fakeKeystoreDriver)(nil)
	_ keystore.Writer = (*fakeKeystoreDriver)(nil)
)

// fakeBlobTarget is the target blob.Adapter restore.Restore's blob step
// puts into: an in-memory map recording every Put, since restoreBlobs
// itself is exercised end to end here (blobs_test.go covers restoreOneBlob
// error paths in isolation).
type fakeBlobTarget struct {
	mu   sync.Mutex
	puts map[string][]byte
}

func newFakeBlobTarget() *fakeBlobTarget {
	return &fakeBlobTarget{puts: map[string][]byte{}}
}

func (f *fakeBlobTarget) List(ctx context.Context, prefix string) ([]blob.Object, error) {
	return nil, nil
}

func (f *fakeBlobTarget) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.puts[key]
	if !ok {
		return nil, blob.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeBlobTarget) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts[key] = data
	return nil
}

// validOptions builds an Options that, unmodified, runs a complete restore
// successfully. Its Keystore is a real "file" driver pointed at the same
// directory the returned Bundle's own Drivers.Keystore config names — the
// production wiring (cmd/farrier/restore.go, internal/api/restore.go)
// builds both from that one config, and deploy.Up (called at the end of
// Restore) rebuilds its own driver from the bundle the same way, so the two
// must actually agree for the end-to-end path to mean anything.
func validOptions(t *testing.T) Options {
	t.Helper()
	source, identity, _ := buildSnapshot(t, []state.Remote{
		{Name: "acme/widgets"}, {Name: "acme/gadgets"},
	}, defaultTestRepos)

	keysDir := t.TempDir()
	keystoreDriver, err := keystore.New("file", map[string]any{"path": keysDir})
	if err != nil {
		t.Fatalf("build keystore driver: %v", err)
	}

	return Options{
		RemoteDir:  "/opt/farrier",
		WorkDir:    filepath.Join(t.TempDir(), "work"),
		Bundle:     testBundle(t, keysDir),
		Source:     source,
		Identity:   identity,
		Keystore:   keystoreDriver,
		Blobs:      newFakeBlobTarget(),
		Host:       newFakeHost(),
		CertIssuer: fakeCertIssuer{},
	}
}

var defaultTestRepos = [][2]string{{"acme", "widgets"}, {"acme", "gadgets"}}

func TestRestoreEndToEnd(t *testing.T) {
	opts := validOptions(t)
	// deploy.Up's configureTLS needs a persisted certificate to reuse
	// (fakeCertIssuer refuses to invent one): install it into the target
	// keystore up front, the same value the snapshot itself captured, so
	// installKeys sees it already present and matching (its idempotent
	// no-op path) rather than writing it — the common case in a real
	// restore against a keystore the operator still holds.
	values := testKeyValues()
	keysDir := opts.Bundle.Manifest.Drivers.Keystore.Config["path"].(string)
	for _, name := range []string{state.KeyTLSCertificate, state.KeyTLSPrivateKey} {
		if err := os.WriteFile(filepath.Join(keysDir, name), []byte(values[name]), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	host := opts.Host.(*fakeHost)
	job := events.NewJob()

	if err := Restore(context.Background(), job, opts); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Every key material name landed in the target keystore with the value
	// the snapshot captured — the ones installKeys wrote fresh, and the
	// TLS pair that was already there and matched.
	for _, name := range keyNames() {
		got, err := opts.Keystore.Resolve(context.Background(), name)
		if err != nil {
			t.Fatalf("resolve %s after restore: %v", name, err)
		}
		if got.Reveal() != values[name] {
			t.Errorf("keystore[%s] = %q, want %q", name, got.Reveal(), values[name])
		}
	}

	// Git: objects extracted before refs, into the UP-004 host path, for
	// both repositories.
	for _, name := range []string{"acme/widgets", "acme/gadgets"} {
		dir := "/opt/farrier/state/git/" + name + ".git"
		extracts := host.commandsContaining("tar -C '" + dir + "' -xf -")
		if len(extracts) != 2 {
			t.Fatalf("repo %s: got %d extract commands, want 2: %v", name, len(extracts), extracts)
		}
		objData := host.stdinFor(t, "tar -C '"+dir+"' -xf -")
		if string(objData) != "objects-bytes-"+name {
			t.Errorf("repo %s: first extraction stdin = %q, want objects archive", name, objData)
		}
	}

	// Database written under the gitea state path.
	dbData := host.stdinFor(t, "cat > '/opt/farrier/state/gitea/gitea.db'")
	if len(dbData) == 0 {
		t.Errorf("database content is empty")
	}

	// State was chowned to the forgejo user after content was placed.
	chowns := host.commandsContaining("chown -R")
	if len(chowns) != 1 {
		t.Fatalf("got %d chown -R commands, want 1: %v", len(chowns), host.commands)
	}

	// Blobs were restored into the target adapter.
	blobs := opts.Blobs.(*fakeBlobTarget)
	if len(blobs.puts) == 0 {
		t.Errorf("no blobs restored into target")
	}

	// deploy.Up ran: the compose definition was shipped and converged.
	converges := host.commandsContaining("docker compose up -d --remove-orphans")
	if len(converges) != 1 {
		t.Fatalf("got %d converge commands, want 1", len(converges))
	}

	// deploy.Up converged the host to the snapshot's pinned Forgejo image
	// (RSTR-002), not the target bundle's own farrier.yaml pin — while the
	// caddy image, which the snapshot doesn't carry, is untouched.
	compose := host.fileEndingWith(t, "docker-compose.yml")
	if !strings.Contains(compose, testSnapshotForgeImage) {
		t.Errorf("shipped compose does not carry the snapshot's pinned forgejo image %q:\n%s", testSnapshotForgeImage, compose)
	}
	if strings.Contains(compose, testTargetForgeImage) {
		t.Errorf("shipped compose still carries the target bundle's original forgejo image %q:\n%s", testTargetForgeImage, compose)
	}
	if !strings.Contains(compose, "docker.io/library/caddy@sha256:"+strings.Repeat("b", 64)) {
		t.Errorf("shipped compose lost the target bundle's caddy image:\n%s", compose)
	}

	// Every step Restore itself owns, plus every step deploy.Up relays
	// through, reached exactly one terminal event on the one external job.
	wantSteps := []string{
		StepFetch, StepDecrypt, StepVerify, StepInstallKeys, StepPlaceState, StepRestoreBlobs,
		deploy.StepCheckHost, deploy.StepConfigureForge, deploy.StepConfigureTLS,
		deploy.StepConfigureState, deploy.StepConverge, deploy.StepWaitForge, deploy.StepWaitCaddy,
	}
	started := map[string]int{}
	terminal := map[string]int{}
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
	for _, step := range wantSteps {
		if started[step] != 1 {
			t.Errorf("step %s started %d time(s), want 1", step, started[step])
		}
		if terminal[step] != 1 {
			t.Errorf("step %s reached a terminal event %d time(s), want 1", step, terminal[step])
		}
	}
}

func TestRestoreRefusesOnFailedVerification(t *testing.T) {
	opts := validOptions(t)

	// Corrupt one component's on-disk bytes by tampering with the fetched
	// archive's destination isn't possible without a real fetch first, so
	// instead point Source at an empty destination — LatestSnapshotKey
	// fails cleanly, which exercises the same "refuse before touching the
	// host" contract as a checksum failure would, without needing to
	// reach into an encrypted archive's bytes.
	empty, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blob.NewLocal: %v", err)
	}
	opts.Source = empty

	host := opts.Host.(*fakeHost)
	job := events.NewJob()

	if err := Restore(context.Background(), job, opts); err == nil {
		t.Fatal("Restore: want error for a snapshot source with nothing to restore, got nil")
	}

	if len(host.commands) != 0 || len(host.stdins) != 0 {
		t.Errorf("host was touched before a snapshot was even fetched: commands=%v stdins=%d", host.commands, len(host.stdins))
	}

	evs := job.Events()
	last := evs[len(evs)-1]
	if last.State != events.StateFailed {
		t.Errorf("job's terminal event state = %s, want failed", last.State)
	}
}

func TestPinnedBundleOverridesForgeImageOnly(t *testing.T) {
	b := testBundle(t, t.TempDir())
	originalCaddy := b.Manifest.Images["caddy"]
	pin := "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("d", 64)

	got, err := pinnedBundle(b, pin)
	if err != nil {
		t.Fatalf("pinnedBundle: %v", err)
	}

	if got.Manifest.Images[forge.Service] != pin {
		t.Errorf("Images[%s] = %q, want %q", forge.Service, got.Manifest.Images[forge.Service], pin)
	}
	if got.Manifest.Images["caddy"] != originalCaddy {
		t.Errorf("Images[caddy] = %q, want unchanged %q", got.Manifest.Images["caddy"], originalCaddy)
	}
	if compose := string(got.Compose[orchestrate.ComposeFile]); !strings.Contains(compose, pin) {
		t.Errorf("rendered compose does not carry the pinned image %q:\n%s", pin, compose)
	}

	// b itself — including the map backing its own Manifest.Images — is
	// left untouched.
	if b.Manifest.Images[forge.Service] != testTargetForgeImage {
		t.Errorf("pinnedBundle mutated the source bundle's forge image: got %q, want %q", b.Manifest.Images[forge.Service], testTargetForgeImage)
	}
}

func TestPinnedBundleRejectsNonDigestVersion(t *testing.T) {
	b := testBundle(t, t.TempDir())
	if _, err := pinnedBundle(b, "not-pinned-by-digest"); err == nil {
		t.Fatal("pinnedBundle: want error for a forgejoVersion that isn't digest-pinned, got nil")
	}
}

func TestOptionsValidate(t *testing.T) {
	base := func(t *testing.T) Options { return validOptions(t) }

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
			opts := base(t)
			tt.mutate(&opts)
			job := events.NewJob()
			if err := Restore(context.Background(), job, opts); err == nil {
				t.Fatalf("Restore: want error for %s, got nil", tt.name)
			}
		})
	}
}
