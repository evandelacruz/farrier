package promote

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
	"github.com/evandelacruz/farrier/internal/core/dns"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/restore"
	"github.com/evandelacruz/farrier/internal/core/state"

	_ "modernc.org/sqlite"
)

// The fixtures below are a trimmed reimplementation of
// internal/core/restore's own test fixtures (its fakes are unexported to
// that package) — just enough to drive a real Promote end to end: a
// snapshot with one repository and one orphaned running CI job, a target
// bundle, a fake host, and a fake DNS driver.

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
// internal/core/restore's own ci_test.go does.
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
// Actions tables forge.ReconcileCI touches, seeded with one repository and
// one orphaned running run/job — exactly what a promotion needs to exercise
// both restore's cross-consistency check and FAIL-003's CI reconciliation.
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

// buildSnapshot runs a real backup.Backup, returning the destination
// adapter Options.Source points at and the identity that decrypts it.
func buildSnapshot(t *testing.T) (blob.Adapter, *age.X25519Identity) {
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
		Database:       &fakeDatabaseExporter{data: newTestDatabaseBytes(t)},
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
// real SSH connection.
type fakeHost struct {
	mu       sync.Mutex
	files    map[string]string
	commands []string
}

func newFakeHost() *fakeHost {
	return &fakeHost{files: make(map[string]string)}
}

func (f *fakeHost) Output(ctx context.Context, command string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	// deploy.ReadStateVersion's read of the recorded forge version: serve it
	// out of the same map WriteFile stores into, so the fake's reads and
	// writes agree the way a real host's do (UPGR-003).
	if rest, ok := strings.CutPrefix(command, "if [ -f '"); ok {
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	return nil
}

func (f *fakeHost) CheckHost(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "docker version")
	return nil
}

func (f *fakeHost) RunStdin(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	if _, err := io.Copy(io.Discard, stdin); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	return nil
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

// fakeDNSDriver records every Set/Delete call, so a test can assert
// Promote flipped the right record to the right value.
type fakeDNSDriver struct {
	mu   sync.Mutex
	sets []dnsSetCall
}

type dnsSetCall struct {
	record string
	value  string
	ttl    time.Duration
}

func (d *fakeDNSDriver) Set(ctx context.Context, record, value string, ttl time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sets = append(d.sets, dnsSetCall{record: record, value: value, ttl: ttl})
	return nil
}

func (d *fakeDNSDriver) Delete(ctx context.Context, record string) error { return nil }

// validOptions builds an Options that, unmodified, runs a complete
// promotion successfully.
func validOptions(t *testing.T) Options {
	t.Helper()
	source, identity := buildSnapshot(t)
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
		DNS:        &fakeDNSDriver{},
		DNSValue:   "203.0.113.10",
	}
}

func TestPromoteEndToEnd(t *testing.T) {
	opts := validOptions(t)
	host := opts.Host.(*fakeHost)
	dnsDriver := opts.DNS.(*fakeDNSDriver)

	job := events.NewJob()
	if err := Promote(context.Background(), job, opts); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if !job.Done() {
		t.Fatal("Promote did not end the job")
	}

	// Services converged (restore.Restore ran all the way through,
	// including deploy.Up).
	if len(host.commandsContaining("docker compose up -d --remove-orphans")) != 1 {
		t.Errorf("want exactly one converge command, got %v", host.commands)
	}

	// The database reached the host (restore's own ci_test.go proves, byte
	// for byte, that ReconcileCI's reset lands in it before this ships).
	if len(host.commandsContaining("cat > '/opt/farrier/state/gitea/gitea.db'")) != 1 {
		t.Errorf("want exactly one database placement command, got %v", host.commands)
	}

	// The DNS flip applied the bundle's own domain to the configured
	// value, at the DNS-004 bundle TTL.
	if len(dnsDriver.sets) != 1 {
		t.Fatalf("got %d DNS Set call(s), want 1: %v", len(dnsDriver.sets), dnsDriver.sets)
	}
	got := dnsDriver.sets[0]
	if got.record != testDomain {
		t.Errorf("dns record = %q, want %q", got.record, testDomain)
	}
	if got.value != opts.DNSValue {
		t.Errorf("dns value = %q, want %q", got.value, opts.DNSValue)
	}
	if got.ttl != dns.BundleTTL {
		t.Errorf("dns ttl = %v, want %v (DNS-004)", got.ttl, dns.BundleTTL)
	}

	// Every restore step relayed through, plus Promote's own DNS-flip
	// step, reached exactly one terminal event on the outer job.
	wantSteps := []string{
		restore.StepFetch, restore.StepDecrypt, restore.StepVerify,
		restore.StepInstallKeys, forge.StepCIReconcile, restore.StepPlaceState,
		restore.StepRestoreBlobs, deploy.StepCheckHost, deploy.StepConfigureForge,
		deploy.StepConfigureTLS, deploy.StepConfigureState, deploy.StepConverge,
		deploy.StepWaitForge, deploy.StepWaitCaddy, StepDNSFlip,
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

func TestPromoteDNSPrintFallback(t *testing.T) {
	opts := validOptions(t)
	// A real caller resolves the DNS driver against the same job it later
	// passes to Promote (ResolveDNSDriver's contract) — PrintDriver reports
	// through whichever job it was built with, not whichever it's later
	// handed to, so the two must match for the fallback to be visible on
	// Promote's own event stream.
	job := events.NewJob()
	opts.DNS = dns.NewPrint(job)

	if err := Promote(context.Background(), job, opts); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	found := false
	for _, ev := range job.Events() {
		if ev.Step == dns.StepDNSChange {
			found = true
			if !strings.Contains(ev.Detail, "no DNS driver configured") {
				t.Errorf("dns-change detail = %q, want the print-fallback message", ev.Detail)
			}
		}
	}
	if !found {
		t.Fatal("no dns-change event found for the print-fallback driver")
	}
}

func TestPromoteCustomDNSRecord(t *testing.T) {
	opts := validOptions(t)
	opts.DNSRecord = "standby.example.com"
	dnsDriver := opts.DNS.(*fakeDNSDriver)

	job := events.NewJob()
	if err := Promote(context.Background(), job, opts); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if len(dnsDriver.sets) != 1 || dnsDriver.sets[0].record != "standby.example.com" {
		t.Fatalf("dns sets = %v, want one Set for standby.example.com", dnsDriver.sets)
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
		{"missing dns driver", func(o *Options) { o.DNS = nil }},
		{"missing dns value", func(o *Options) { o.DNSValue = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := base(t)
			tt.mutate(&opts)
			job := events.NewJob()
			if err := Promote(context.Background(), job, opts); err == nil {
				t.Fatalf("Promote: want error for %s, got nil", tt.name)
			}
		})
	}
}
