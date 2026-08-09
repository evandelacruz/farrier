package upgrade

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// testDatabaseBytes builds a real, queryable, empty-of-rows SQLite file
// with the three tables backup.Verify's cross-consistency check reads —
// enough for a captured database to open and pass verification with zero
// repositories, zero LFS objects, and zero attachments.
func testDatabaseBytes(t *testing.T) []byte {
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
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
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

func mustLocalBlob(t *testing.T, dir string) blob.Adapter {
	t.Helper()
	a, err := blob.NewLocal(dir)
	if err != nil {
		t.Fatalf("blob.NewLocal: %v", err)
	}
	return a
}

type fakeCertIssuer struct{}

func (fakeCertIssuer) EnsureValid(cfg acme.Config, existing *acme.Certificate, now time.Time) (*acme.Certificate, bool, error) {
	if existing == nil {
		return nil, false, errors.New("fakeCertIssuer: no persisted certificate to reuse")
	}
	return existing, false, nil
}

func testBundle(t *testing.T, keysDir, blobsDir string) *bundle.Bundle {
	t.Helper()
	return &bundle.Bundle{
		Manifest: *bundle.NewManifest(testDomain, map[string]string{
			"forgejo": "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("a", 64),
			"caddy":   "docker.io/library/caddy@sha256:" + strings.Repeat("b", 64),
		}, bundle.DriverConfig{
			Keystore: bundle.DriverRef{Driver: "file", Config: map[string]any{"path": keysDir}},
			Blob:     bundle.DriverRef{Driver: "local", Config: map[string]any{"path": blobsDir}},
		}, bundle.ACMEConfig{DNSProvider: "manual", Email: "ops@example.com"}),
		Compose: map[string][]byte{
			"docker-compose.yml": []byte("services:\n  forgejo:\n    image: x\n  caddy:\n    image: y\n"),
		},
	}
}

// fakeHost implements Host without a real SSH connection: it records every
// command it's given and answers just enough of them — docker compose ps,
// df, and the sqlite3 backup command SSHDatabaseExporter shells out to —
// for both status.Check and a full backup.Backup/deploy.Up sequence to
// succeed against it.
type fakeHost struct {
	mu       sync.Mutex
	files    map[string]string
	commands []string

	dbBytes      []byte
	servicesDown bool
	diskFull     bool
}

func newFakeHost(dbBytes []byte) *fakeHost {
	return &fakeHost{files: make(map[string]string), dbBytes: dbBytes}
}

func (f *fakeHost) Target() orchestrate.Target {
	return orchestrate.Target{User: "root", Host: "forge.example.com", Port: "22"}
}

func (f *fakeHost) Output(ctx context.Context, command string) ([]byte, error) {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	f.mu.Unlock()

	switch {
	case strings.Contains(command, "docker compose ps"):
		state := "running"
		if f.servicesDown {
			state = "exited"
		}
		return []byte(fmt.Sprintf(
			`[{"Service":"forgejo","State":%q,"Status":"container state"},{"Service":"caddy","State":%q,"Status":"container state"}]`,
			state, state)), nil
	case strings.HasPrefix(command, "df -Pk"):
		available := "90000000"
		if f.diskFull {
			available = "0"
		}
		return []byte("Filesystem 1024-blocks Used Available Capacity Mounted\n/dev/sda1 100000000 10000000 " + available + " 10% /\n"), nil
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
	f.commands = append(f.commands, command)
	f.mu.Unlock()
	if strings.Contains(command, "sqlite3") {
		stdout.Write(f.dbBytes)
	}
	return nil
}

func (f *fakeHost) CheckHost(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "docker version")
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

// fakeResolver pins every ref to a deterministic digest derived from the
// ref itself, so tests can assert on the resulting manifest without a real
// registry — the same fixture initialize_test.go uses for the same reason.
type fakeResolver struct {
	calls []string
	err   error
}

func (f *fakeResolver) Resolve(ctx context.Context, ref string) (string, error) {
	f.calls = append(f.calls, ref)
	if f.err != nil {
		return "", f.err
	}
	name := ref
	if i := strings.IndexAny(name, ":@"); i != -1 {
		name = name[:i]
	}
	return fmt.Sprintf("%s@sha256:%s", name, strings.Repeat("f", 64)), nil
}

// validOptions builds an Options that, unmodified, runs a complete upgrade
// successfully, plus the bundle directory it was saved to (Options.Bundle
// is loaded from it) so a test can inspect what Upgrade persisted.
func validOptions(t *testing.T) (Options, string) {
	t.Helper()
	keysDir := t.TempDir()
	values := testKeyValues()
	for name, value := range values {
		if err := os.WriteFile(filepath.Join(keysDir, name), []byte(value), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	blobsDir := t.TempDir()
	bundleDir := filepath.Join(t.TempDir(), "bundle")
	b := testBundle(t, keysDir, blobsDir)
	if err := b.Save(bundleDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := bundle.Load(bundleDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	keystoreDriver, err := keystore.New("file", map[string]any{"path": keysDir})
	if err != nil {
		t.Fatalf("build keystore driver: %v", err)
	}
	blobAdapter := mustLocalBlob(t, blobsDir)

	opts := Options{
		BundleDir:   bundleDir,
		RemoteDir:   "/opt/farrier",
		WorkDir:     filepath.Join(t.TempDir(), "work"),
		Bundle:      loaded,
		Destination: filepath.Join(t.TempDir(), "backups"),
		NewImage:    "codeberg.org/forgejo/forgejo:1.22",
		Identity:    mustAgeIdentity(t),
		Keystore:    keystoreDriver,
		Blobs:       blobAdapter,
		Host:        newFakeHost(testDatabaseBytes(t)),
		CertIssuer:  fakeCertIssuer{},
		Resolver:    &fakeResolver{},
	}
	return opts, bundleDir
}

func mustAgeIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	return identity
}

func TestUpgradeEndToEnd(t *testing.T) {
	opts, bundleDir := validOptions(t)
	host := opts.Host.(*fakeHost)

	job := events.NewJob()
	if err := Upgrade(context.Background(), job, opts); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if !job.Done() {
		t.Fatal("Upgrade did not end the job")
	}

	// The bumped manifest was saved back to BundleDir (CORE-001 bundle
	// content): a fresh Load sees the new pin, not the old one.
	reloaded, err := bundle.Load(bundleDir)
	if err != nil {
		t.Fatalf("reload bundle: %v", err)
	}
	wantImage := "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("f", 64)
	if got := reloaded.Manifest.Images[forge.Service]; got != wantImage {
		t.Errorf("saved forgejo image = %q, want %q", got, wantImage)
	}
	if !strings.Contains(string(reloaded.Compose["docker-compose.yml"]), wantImage) {
		t.Errorf("saved compose does not reference the bumped image: %s", reloaded.Compose["docker-compose.yml"])
	}

	// The pre-upgrade backup exists at the named destination.
	entries, err := os.ReadDir(opts.Destination)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if len(entries) == 0 {
		t.Error("no pre-upgrade backup written to the destination")
	}

	// deploy.Up converged the host to the bumped definition.
	if len(host.commandsContaining("docker compose up -d --remove-orphans")) != 1 {
		t.Errorf("want exactly one converge command, got %v", host.commands)
	}

	// Every delegated step, plus Upgrade's own, reached exactly one
	// terminal event on the outer job.
	wantSteps := []string{
		StepCheckHealth,
		backup.StepValidate, backup.StepPushHold, backup.StepDatabase, backup.StepRecordRefs,
		backup.StepGit, backup.StepBlobs, backup.StepKeys, backup.StepWriteManifest, backup.StepVerify,
		backup.StepResolveDestination, backup.StepVerifyEncrypted,
		StepBumpVersion,
		deploy.StepCheckHost, deploy.StepConfigureForge, deploy.StepConfigureTLS,
		deploy.StepConfigureState, deploy.StepConfigureSSHKey, deploy.StepConverge,
		deploy.StepWaitForge, deploy.StepWaitCaddy,
		StepVerify,
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

// TestUpgradeBackupPinsPreUpgradeVersion proves the load-bearing ordering
// UPGR-003 depends on: the pre-upgrade backup's manifest records the
// version the instance was running *before* the bump, never the one it's
// upgrading to — otherwise a restore from that snapshot would try to boot
// an image it never actually ran and skip the migrations that got it
// there.
func TestUpgradeBackupPinsPreUpgradeVersion(t *testing.T) {
	opts, _ := validOptions(t)
	preUpgradeVersion := opts.Bundle.Manifest.Images[forge.Service]

	job := events.NewJob()
	if err := Upgrade(context.Background(), job, opts); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}

	source, err := blob.NewLocal(opts.Destination)
	if err != nil {
		t.Fatalf("open destination: %v", err)
	}
	key, err := backup.LatestSnapshotKey(context.Background(), source)
	if err != nil {
		t.Fatalf("LatestSnapshotKey: %v", err)
	}
	archivePath := filepath.Join(t.TempDir(), "snapshot.age")
	if err := backup.Fetch(context.Background(), source, key, archivePath); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	plainDir := t.TempDir()
	if err := backup.DecryptArchive(context.Background(), archivePath, plainDir, opts.Identity); err != nil {
		t.Fatalf("DecryptArchive: %v", err)
	}
	manifest, err := backup.ReadManifest(plainDir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if manifest.ForgejoVersion != preUpgradeVersion {
		t.Errorf("snapshot ForgejoVersion = %q, want the pre-upgrade pin %q", manifest.ForgejoVersion, preUpgradeVersion)
	}
}

func TestUpgradeRefusesUnhealthyInstance(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeHost)
		want   string
	}{
		{"services down", func(h *fakeHost) { h.servicesDown = true }, "service"},
		{"disk full", func(h *fakeHost) { h.diskFull = true }, "disk headroom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, bundleDir := validOptions(t)
			host := opts.Host.(*fakeHost)
			tt.mutate(host)

			job := events.NewJob()
			err := Upgrade(context.Background(), job, opts)
			if err == nil {
				t.Fatal("Upgrade: want error for an unhealthy instance, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Upgrade error = %q, want it to mention %q", err.Error(), tt.want)
			}

			// Refused before anything changed: no backup was written, and
			// the bundle on disk still pins the original image.
			if entries, _ := os.ReadDir(opts.Destination); len(entries) != 0 {
				t.Error("a backup was written despite the health gate refusing")
			}
			reloaded, loadErr := bundle.Load(bundleDir)
			if loadErr != nil {
				t.Fatalf("reload bundle: %v", loadErr)
			}
			if reloaded.Manifest.Images[forge.Service] != opts.Bundle.Manifest.Images[forge.Service] {
				t.Error("bundle manifest was bumped despite the health gate refusing")
			}
		})
	}
}

func TestUpgradeResolverFailureLeavesBundleUnchanged(t *testing.T) {
	opts, bundleDir := validOptions(t)
	opts.Resolver = &fakeResolver{err: errors.New("registry unreachable")}

	job := events.NewJob()
	if err := Upgrade(context.Background(), job, opts); err == nil {
		t.Fatal("Upgrade: want error when the image resolver fails, got nil")
	}

	// The pre-upgrade backup still ran and is still on disk (UPGR-002): a
	// resolver failure happens after the backup, not instead of it.
	entries, err := os.ReadDir(opts.Destination)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if len(entries) == 0 {
		t.Error("pre-upgrade backup missing after a resolver failure")
	}

	reloaded, err := bundle.Load(bundleDir)
	if err != nil {
		t.Fatalf("reload bundle: %v", err)
	}
	if reloaded.Manifest.Images[forge.Service] != opts.Bundle.Manifest.Images[forge.Service] {
		t.Error("bundle manifest was bumped despite the resolver failing")
	}
}

func TestOptionsValidate(t *testing.T) {
	base := func(t *testing.T) Options { opts, _ := validOptions(t); return opts }

	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{"missing bundle dir", func(o *Options) { o.BundleDir = "" }},
		{"missing remote dir", func(o *Options) { o.RemoteDir = "" }},
		{"missing work dir", func(o *Options) { o.WorkDir = "" }},
		{"missing bundle", func(o *Options) { o.Bundle = nil }},
		{"missing destination", func(o *Options) { o.Destination = "" }},
		{"missing new image", func(o *Options) { o.NewImage = "" }},
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
			if err := Upgrade(context.Background(), job, opts); err == nil {
				t.Fatalf("Upgrade: want error for %s, got nil", tt.name)
			}
		})
	}
}
