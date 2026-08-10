package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/acme"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/caddy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// fakeCertIssuer satisfies CertIssuer without a real ACME server or DNS
// provider, so tests can exercise Up's TLS-provisioning step
// deterministically. By default it always issues (calls is appended and a
// fresh fake certificate returned), matching what a caller with no
// persisted certificate would see. Setting reuse makes it instead mimic
// acme.EnsureValid's short-circuit: existing is returned unchanged and
// calls is left untouched, so tests can assert nothing reached "the ACME
// server" when a persisted certificate is still fresh (UP-003).
type fakeCertIssuer struct {
	calls        []acme.Config
	existingSeen []*acme.Certificate
	err          error
	reuse        bool
}

func (f *fakeCertIssuer) EnsureValid(cfg acme.Config, existing *acme.Certificate, now time.Time) (*acme.Certificate, bool, error) {
	f.existingSeen = append(f.existingSeen, existing)
	if f.reuse {
		if existing == nil {
			return nil, false, errors.New("fakeCertIssuer: reuse requested but no existing certificate")
		}
		return existing, false, nil
	}

	f.calls = append(f.calls, cfg)
	if f.err != nil {
		return nil, false, f.err
	}
	return &acme.Certificate{
		Domain:      cfg.Domain,
		Certificate: []byte("fake-cert-pem"),
		PrivateKey:  []byte("fake-key-pem"),
	}, true, nil
}

// testOptions is Options with a fakeCertIssuer wired in, for tests whose
// Up call runs far enough to reach TLS provisioning.
func testOptions(remoteDir string) Options {
	return Options{RemoteDir: remoteDir, CertIssuer: &fakeCertIssuer{}}
}

func init() {
	readyInterval = time.Millisecond
}

// fakeHost implements Host without a real SSH server, recording every
// command it's asked to run so tests can assert on Up's sequencing.
type fakeHost struct {
	mu sync.Mutex

	files    map[string]string
	commands []string

	checkHostErr      error
	writeFileErr      error
	execFailures      int // number of leading `docker compose exec ... true` calls that fail
	adminCreateErr    error
	adminCreateStderr string

	// forgeReadyFailures is how many leading forgejo readiness probes
	// (`forgejo admin user list`) fail before one succeeds. It is separate
	// from execFailures on purpose: the two model the two things that are
	// true at different moments on a fresh host — the container accepts an
	// exec well before Forgejo has finished creating its database schema —
	// and the bug this fake exists to catch lives in the gap between them.
	forgeReadyFailures int

	// forgeReadyStderr is what a failing readiness probe writes to stderr,
	// so a test can assert the operator is told what Forgejo said.
	forgeReadyStderr string

	// forgeLog is what `docker compose logs ... forgejo` prints, standing in
	// for the container log a stalled deployment reports back.
	forgeLog string

	// readVersionErr fails the read of the recorded forge version
	// (stateversion.go), standing in for an unreadable host.
	readVersionErr error

	// failOutputOn fails any Output whose command contains it, standing in
	// for a host that refuses one specific command — how Down's tests drive
	// a failure at a chosen step of the teardown.
	failOutputOn string

	// versionAtConverge is the recorded forge version as it stood the
	// moment `docker compose up` ran — what lets a test assert Up records
	// the version it is about to start *before* starting it.
	versionAtConverge string
}

func newFakeHost() *fakeHost {
	return &fakeHost{files: make(map[string]string)}
}

func (f *fakeHost) Output(ctx context.Context, command string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)

	if f.failOutputOn != "" && strings.Contains(command, f.failOutputOn) {
		return nil, errors.New("fakeHost: command failed: " + command)
	}

	// Serve reads of the recorded forge version out of the same map
	// WriteFile stores into, so the fake's reads and writes agree the way
	// a real host's do — without that, every Up here would see an empty
	// record and UPGR-003's check could never be exercised.
	if p := stateVersionRead(command); p != "" {
		if f.readVersionErr != nil {
			return nil, f.readVersionErr
		}
		return []byte(f.files[p]), nil
	}
	if strings.Contains(command, "docker compose up") {
		f.versionAtConverge = f.files[StateVersionPath("/opt/farrier")]
	}
	return nil, nil
}

// stateVersionRead returns the path a command reads the recorded forge
// version from, or "" if it isn't that command — matching the shape
// ReadStateVersion builds.
func stateVersionRead(command string) string {
	rest, ok := strings.CutPrefix(command, "if [ -f '")
	if !ok {
		return ""
	}
	p, _, _ := strings.Cut(rest, "'")
	return p
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

	if strings.Contains(command, "exec -T forgejo true") {
		if f.execFailures > 0 {
			f.execFailures--
			return errors.New("container not ready")
		}
		return nil
	}
	if strings.Contains(command, "admin user list") {
		if f.forgeReadyFailures > 0 {
			f.forgeReadyFailures--
			if f.forgeReadyStderr != "" && stderr != nil {
				stderr.Write([]byte(f.forgeReadyStderr))
			}
			return errors.New("forgejo not ready")
		}
		return nil
	}
	if strings.Contains(command, "docker compose logs") {
		if f.forgeLog != "" && stdout != nil {
			stdout.Write([]byte(f.forgeLog))
		}
		return nil
	}
	if strings.Contains(command, "admin user create") {
		if f.adminCreateStderr != "" && stderr != nil {
			stderr.Write([]byte(f.adminCreateStderr))
		}
		return f.adminCreateErr
	}
	return nil
}

func (f *fakeHost) CheckHost(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "docker version")
	return f.checkHostErr
}

// testBundle returns a bundle whose keystore points at a fresh copy of
// testdata/keys, private to the calling test. Up's TLS step can now
// persist a renewed certificate back through the keystore (ACME-002), so
// tests must not point the "file" driver straight at the checked-in
// fixture — writing there would corrupt it for every other test and leave
// the working tree dirty after a single `go test` run. A t.TempDir() path
// is already absolute, which also satisfies the file driver's XCUT-001
// requirement for free.
func testBundle(t *testing.T) *bundle.Bundle {
	t.Helper()
	keysDir := t.TempDir()
	copyFixtureFiles(t, "testdata/keys", keysDir)
	return &bundle.Bundle{
		Manifest: *bundle.NewManifest("example.com", map[string]string{
			"forgejo": "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("a", 64),
			"caddy":   "docker.io/library/caddy@sha256:" + strings.Repeat("b", 64),
		}, bundle.DriverConfig{
			Keystore: bundle.DriverRef{Driver: "file", Config: map[string]any{"path": keysDir}},
			Blob:     bundle.DriverRef{Driver: "local", Config: map[string]any{"path": "testdata/blobs"}},
		}, bundle.ACMEConfig{DNSProvider: "manual", Email: "ops@example.com"}),
		Compose: map[string][]byte{
			"docker-compose.yml": []byte("services:\n  forgejo:\n    image: x\n  caddy:\n    image: y\n"),
		},
	}
}

// namelessBundle returns what `init` with no domain produces (INIT-005):
// testBundle with the domain and the ACME section it pairs with both
// dropped, which is the shape bundle.Manifest.Validate accepts as nameless.
// Everything else about the bundle is unchanged — a nameless instance is a
// complete instance in all respects but its name (spec.md "Instances
// without a name").
func namelessBundle(t *testing.T) *bundle.Bundle {
	t.Helper()
	b := testBundle(t)
	b.Manifest.Domain = ""
	b.Manifest.ACME = bundle.ACMEConfig{}
	if err := b.Manifest.Validate(); err != nil {
		t.Fatalf("nameless manifest is not valid: %v", err)
	}
	return b
}

// copyFixtureFiles copies every file directly under src into dst, so a
// test gets its own writable copy of a checked-in fixture directory.
func copyFixtureFiles(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			t.Fatalf("read %s/%s: %v", src, entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, entry.Name()), data, 0o600); err != nil {
			t.Fatalf("write %s/%s: %v", dst, entry.Name(), err)
		}
	}
}

// keystorePath returns the directory b's "file" keystore driver was
// configured with, so a test can inspect what Up persisted there.
func keystorePath(t *testing.T, b *bundle.Bundle) string {
	t.Helper()
	path, ok := b.Manifest.Drivers.Keystore.Config["path"].(string)
	if !ok {
		t.Fatal("bundle keystore driver has no string \"path\" config")
	}
	return path
}

func drain(job *events.Job) []events.Event {
	ch, cancel := job.Subscribe()
	defer cancel()
	var out []events.Event
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func TestUpSucceeds(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()
	issuer := &fakeCertIssuer{}

	err := Up(context.Background(), job, host, testBundle(t), Options{RemoteDir: "/opt/farrier", CertIssuer: issuer})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	evs := drain(job)
	if len(evs) == 0 {
		t.Fatal("Up: emitted no events")
	}
	last := evs[len(evs)-1]
	if last.State != events.StateSucceeded || last.Step != "" {
		t.Errorf("last event = %+v, want a job-terminal success", last)
	}

	appINI, ok := host.files["/opt/farrier/forge/app.ini"]
	if !ok {
		t.Fatalf("app.ini not shipped, wrote: %v", keysOf(host.files))
	}
	if !strings.Contains(appINI, "INSTALL_LOCK = true") {
		t.Errorf("shipped app.ini missing INSTALL_LOCK: %s", appINI)
	}

	if len(issuer.calls) != 1 {
		t.Fatalf("cert issuer calls = %d, want 1", len(issuer.calls))
	}
	if got := issuer.calls[0]; got.Domain != "example.com" || got.DNSProvider != "manual" || got.Email != "ops@example.com" {
		t.Errorf("issue call = %+v, want domain/provider/email from the manifest's ACME config", got)
	}
	if issuer.calls[0].AccountKey == nil {
		t.Error("issue call carries no account key")
	}
	if len(issuer.existingSeen) != 1 || issuer.existingSeen[0] == nil {
		t.Fatalf("existingSeen = %+v, want the persisted testdata certificate resolved and passed through", issuer.existingSeen)
	}
	if issuer.existingSeen[0].NotAfter.IsZero() {
		t.Error("existingSeen[0] carries a zero NotAfter — persisted certificate was not parsed")
	}

	caddyfile, ok := host.files["/opt/farrier/caddy/Caddyfile"]
	if !ok {
		t.Fatalf("caddyfile not shipped, wrote: %v", keysOf(host.files))
	}
	if !strings.Contains(caddyfile, "example.com {") || !strings.Contains(caddyfile, "reverse_proxy forgejo:3000") {
		t.Errorf("shipped caddyfile missing domain/upstream: %s", caddyfile)
	}
	if host.files["/opt/farrier/caddy/tls.crt"] != "fake-cert-pem" {
		t.Errorf("certificate not shipped, wrote: %v", keysOf(host.files))
	}
	if host.files["/opt/farrier/caddy/tls.key"] != "fake-key-pem" {
		t.Errorf("private key not shipped, wrote: %v", keysOf(host.files))
	}

	var sawCheckHost, sawComposeUp, sawExecForgejoReady, sawForgeDBReady, sawExecCaddyReady, sawAdminCreate bool
	for _, cmd := range host.commands {
		switch {
		case strings.Contains(cmd, "docker version"):
			sawCheckHost = true
		case strings.Contains(cmd, "docker compose up -d"):
			sawComposeUp = true
			if !strings.Contains(cmd, "COMPOSE_PROJECT_NAME=farrier") {
				t.Errorf("compose up command missing project name: %q", cmd)
			}
		case strings.Contains(cmd, "exec -T forgejo true"):
			sawExecForgejoReady = true
		case strings.Contains(cmd, "admin user list"):
			sawForgeDBReady = true
		case strings.Contains(cmd, "exec -T caddy true"):
			sawExecCaddyReady = true
		case strings.Contains(cmd, "admin user create"):
			sawAdminCreate = true
			if !strings.Contains(cmd, "COMPOSE_PROJECT_NAME=farrier") {
				t.Errorf("admin create command missing project name: %q", cmd)
			}
		}
	}
	if !sawCheckHost || !sawComposeUp || !sawExecForgejoReady || !sawForgeDBReady || !sawExecCaddyReady || !sawAdminCreate {
		t.Fatalf("missing a step: checkHost=%v composeUp=%v execForgejoReady=%v forgeDBReady=%v execCaddyReady=%v adminCreate=%v (commands: %v)",
			sawCheckHost, sawComposeUp, sawExecForgejoReady, sawForgeDBReady, sawExecCaddyReady, sawAdminCreate, host.commands)
	}
}

func TestUpPublishesCaddyHTTPSPort(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()

	if err := Up(context.Background(), job, host, testBundle(t), testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	var sawComposeUp bool
	for _, cmd := range host.commands {
		if strings.Contains(cmd, "docker compose up -d") {
			sawComposeUp = true
		}
	}
	if !sawComposeUp {
		t.Fatal("Up: never ran docker compose up")
	}

	composed, ok := host.files["/opt/farrier/compose.tmp/docker-compose.yml"]
	if !ok {
		t.Fatalf("compose file not shipped, wrote: %v", keysOf(host.files))
	}
	if !strings.Contains(composed, "443:443") {
		t.Errorf("shipped compose missing caddy's published https port: %s", composed)
	}
	if !strings.Contains(composed, caddy.ConfigPath) || !strings.Contains(composed, caddy.CertPath) || !strings.Contains(composed, caddy.KeyPath) {
		t.Errorf("shipped compose missing caddy's bind mounts: %s", composed)
	}
}

func TestUpFailsWhenCertIssuanceFails(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()
	issuer := &fakeCertIssuer{err: errors.New("dns-01 challenge failed")}

	err := Up(context.Background(), job, host, testBundle(t), Options{RemoteDir: "/opt/farrier", CertIssuer: issuer})
	if err == nil {
		t.Fatal("Up: want error when certificate issuance fails, got nil")
	}
	if !strings.Contains(err.Error(), "dns-01 challenge failed") {
		t.Errorf("error = %v, want it to wrap the issuer's reason", err)
	}

	evs := drain(job)
	last := evs[len(evs)-1]
	if last.State != events.StateFailed || last.Step != "" {
		t.Errorf("last event = %+v, want a job-terminal failure", last)
	}
	for _, cmd := range host.commands {
		if strings.Contains(cmd, "docker compose up -d") {
			t.Error("Up ran docker compose up after certificate issuance failed")
		}
	}
}

// TestUpReusesPersistedCertificateWithoutIssuing exercises the core of
// UP-003's TLS fix: re-running Up against a host with a persisted
// certificate that isn't due for renewal must not reach the ACME server at
// all, or a handful of re-runs would trip Let's Encrypt's
// duplicate-certificate rate limit.
func TestUpReusesPersistedCertificateWithoutIssuing(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()
	issuer := &fakeCertIssuer{reuse: true}
	b := testBundle(t)

	if err := Up(context.Background(), job, host, b, Options{RemoteDir: "/opt/farrier", CertIssuer: issuer}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if len(issuer.calls) != 0 {
		t.Errorf("cert issuer calls = %d, want 0 — a fresh persisted certificate must not be reissued", len(issuer.calls))
	}

	persistedCert, err := os.ReadFile(filepath.Join(keystorePath(t, b), "tls_certificate"))
	if err != nil {
		t.Fatalf("read persisted certificate: %v", err)
	}
	if host.files["/opt/farrier/caddy/tls.crt"] != string(persistedCert) {
		t.Error("shipped certificate does not match the persisted certificate")
	}

	evs := drain(job)
	var sawReuse bool
	for _, ev := range evs {
		if ev.Step == StepConfigureTLS && ev.State == events.StateSucceeded && strings.Contains(ev.Detail, "reused") {
			sawReuse = true
		}
	}
	if !sawReuse {
		t.Errorf("no configure-tls success event reporting reuse, events: %+v", evs)
	}
}

// TestUpPersistsRenewedCertificate exercises ACME-002's close: on the rare
// branch where the persisted certificate is due for renewal, Up must not
// only use the freshly issued certificate for this deploy but also write
// it back to the keystore, so the next Up sees the renewal instead of
// deciding the same certificate is due all over again.
func TestUpPersistsRenewedCertificate(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()
	issuer := &fakeCertIssuer{}
	b := testBundle(t)

	if err := Up(context.Background(), job, host, b, Options{RemoteDir: "/opt/farrier", CertIssuer: issuer}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if host.files["/opt/farrier/caddy/tls.crt"] != "fake-cert-pem" {
		t.Error("Up did not ship the freshly issued certificate for this deploy")
	}

	gotCert, err := os.ReadFile(filepath.Join(keystorePath(t, b), "tls_certificate"))
	if err != nil {
		t.Fatalf("read persisted certificate: %v", err)
	}
	if string(gotCert) != "fake-cert-pem" {
		t.Errorf("persisted certificate = %q, want the renewed certificate", gotCert)
	}
	gotKey, err := os.ReadFile(filepath.Join(keystorePath(t, b), "tls_private_key"))
	if err != nil {
		t.Fatalf("read persisted private key: %v", err)
	}
	if string(gotKey) != "fake-key-pem" {
		t.Errorf("persisted private key = %q, want the renewed private key", gotKey)
	}

	evs := drain(job)
	var sawPersisted bool
	for _, ev := range evs {
		if ev.Step == StepConfigureTLS && ev.State == events.StateSucceeded && strings.Contains(ev.Detail, "persisted") {
			sawPersisted = true
		}
	}
	if !sawPersisted {
		t.Errorf("no configure-tls success event reporting the renewal was persisted, events: %+v", evs)
	}
}

// TestUpFailsWhenKeystoreCannotPersistRenewedCertificate exercises the
// defensive branch in persistRenewedCertificate: init requires a
// Writer-capable keystore driver (initialize.Run), but if a bundle's
// keystore target is later reconfigured to one that isn't (e.g.
// "command", read-only by design per KEY-002), a renewal due at deploy
// time must fail clearly rather than silently serve a certificate it
// can't save.
func TestUpFailsWhenKeystoreCannotPersistRenewedCertificate(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()
	issuer := &fakeCertIssuer{}
	b := testBundle(t)
	b.Manifest.Drivers.Keystore = bundle.DriverRef{
		Driver: "command",
		Config: map[string]any{"command": `cat testdata/keys/"$FARRIER_KEY_NAME"`},
	}

	err := Up(context.Background(), job, host, b, Options{RemoteDir: "/opt/farrier", CertIssuer: issuer})
	if err == nil {
		t.Fatal("Up: want error when the keystore driver cannot persist a renewed certificate, got nil")
	}
	if !strings.Contains(err.Error(), "persist renewed") {
		t.Errorf("error = %v, want it to name the persistence failure", err)
	}
}

func TestUpRetriesUntilForgejoReady(t *testing.T) {
	host := newFakeHost()
	host.execFailures = 2
	job := events.NewJob()

	if err := Up(context.Background(), job, host, testBundle(t), testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	count := 0
	for _, cmd := range host.commands {
		if strings.Contains(cmd, "exec -T forgejo true") {
			count++
		}
	}
	if count != 3 {
		t.Errorf("readiness probes = %d, want 3 (2 failures + 1 success)", count)
	}
}

// The bug this exists to catch: on a host whose state directory is fresh,
// the forgejo container accepts an exec seconds before Forgejo has finished
// creating its database schema, and an admin account created in that window
// fails with "no such table: user". The container accepting an exec is
// therefore not the condition Up may bootstrap on.
func TestUpWaitsForForgejoDatabaseBeforeBootstrapping(t *testing.T) {
	host := newFakeHost()
	host.execFailures = 0 // the container is up immediately, as it is in the real failure
	host.forgeReadyFailures = 3
	host.forgeReadyStderr = "Command error: CreateUser: no such table: user"

	if err := Up(context.Background(), events.NewJob(), host, testBundle(t), testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	probes := 0
	lastProbe, create := -1, -1
	for i, cmd := range host.commands {
		switch {
		case strings.Contains(cmd, "admin user list"):
			probes++
			lastProbe = i
		case strings.Contains(cmd, "admin user create"):
			if create < 0 {
				create = i
			}
		}
	}
	if probes != 4 {
		t.Errorf("database probes = %d, want 4 (3 failures + 1 success); commands: %v", probes, host.commands)
	}
	if create < 0 {
		t.Fatalf("no admin account was created; commands: %v", host.commands)
	}
	if lastProbe > create {
		t.Errorf("admin bootstrap ran at %d, before the database was ready at %d; commands: %v", create, lastProbe, host.commands)
	}
}

// A Forgejo that never finishes — an unwritable state directory, a bad
// image, a config it refuses — has to fail the step saying what was being
// waited for, and hand over the container log the reason is actually in.
func TestUpFailsWhenForgejoNeverFinishesItsDatabase(t *testing.T) {
	shortenForgeReadyTimeout(t)

	host := newFakeHost()
	host.forgeReadyFailures = 1 << 30
	host.forgeReadyStderr = "Command error: CreateUser: no such table: user"
	host.forgeLog = "boot: failed to open provider: permission denied"
	job := events.NewJob()

	err := Up(context.Background(), job, host, testBundle(t), testOptions("/opt/farrier"))
	if err == nil {
		t.Fatal("Up: want error when forgejo never finishes setting up its database, got nil")
	}
	for _, want := range []string{"did not finish setting up its database", "no such table: user", host.forgeLog} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	if ranCommandContaining(host, "admin user create") {
		t.Errorf("admin bootstrap ran against a forge that was never ready; commands: %v", host.commands)
	}

	evs := drain(job)
	if last := evs[len(evs)-1]; last.State != events.StateFailed {
		t.Fatalf("terminal event state = %v, want failed", last.State)
	}
	var detail string
	for _, ev := range evs {
		if ev.Step == StepWaitForge && ev.State == events.StateFailed {
			detail = ev.Detail
		}
	}
	if !strings.Contains(detail, "did not finish setting up its database") {
		t.Errorf("%s detail = %q, want it to name what was being waited for", StepWaitForge, detail)
	}
	if !strings.Contains(detail, host.forgeLog) {
		t.Errorf("%s detail = %q, want it to carry the container log", StepWaitForge, detail)
	}
}

// KEY-003: the container log is Forgejo's output, and Forgejo was handed the
// bundle's key material in app.ini. Whatever it chooses to echo, none of it
// reaches an event.
func TestUpKeepsKeyMaterialOutOfTheReportedForgejoLog(t *testing.T) {
	shortenForgeReadyTimeout(t)

	host := newFakeHost()
	host.forgeReadyFailures = 1 << 30
	host.forgeLog = "config: [security] SECRET_KEY = test-secret-key-value is not valid"

	err := Up(context.Background(), events.NewJob(), host, testBundle(t), testOptions("/opt/farrier"))
	if err == nil {
		t.Fatal("Up: want error when forgejo never finishes setting up its database, got nil")
	}
	if strings.Contains(err.Error(), "test-secret-key-value") {
		t.Errorf("error = %q, want the forgejo secret key redacted out of the reported log", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("error = %q, want the redacted stand-in in place of the secret", err)
	}
}

// shortenForgeReadyTimeout drops the forge wait's budget to something a test
// can afford, restoring it afterwards. The real budget is three minutes.
func shortenForgeReadyTimeout(t *testing.T) {
	t.Helper()
	original := forgeReadyTimeout
	forgeReadyTimeout = 20 * time.Millisecond
	t.Cleanup(func() { forgeReadyTimeout = original })
}

func TestUpFailsWhenDockerUnreachable(t *testing.T) {
	host := newFakeHost()
	host.checkHostErr = errors.New("no docker")
	job := events.NewJob()

	err := Up(context.Background(), job, host, testBundle(t), Options{RemoteDir: "/opt/farrier"})
	if err == nil {
		t.Fatal("Up: want error when Docker is unreachable, got nil")
	}

	evs := drain(job)
	last := evs[len(evs)-1]
	if last.State != events.StateFailed || last.Step != "" {
		t.Errorf("last event = %+v, want a job-terminal failure", last)
	}
	if len(host.commands) != 1 || host.commands[0] != "docker version" {
		t.Errorf("commands past the host check: %v, want only the check itself", host.commands)
	}
}

func TestUpFailsWhenAdminCreateFails(t *testing.T) {
	host := newFakeHost()
	host.adminCreateErr = errors.New("create failed")
	job := events.NewJob()

	if err := Up(context.Background(), job, host, testBundle(t), testOptions("/opt/farrier")); err == nil {
		t.Fatal("Up: want error when admin bootstrap fails, got nil")
	}
}

// TestUpSucceedsWhenAlreadyDeployed exercises UP-003 end to end: re-running
// Up against a host it already deployed to must not fail just because the
// admin account it provisioned last time is still there.
func TestUpSucceedsWhenAlreadyDeployed(t *testing.T) {
	host := newFakeHost()
	host.adminCreateErr = errors.New("exit status 1")
	host.adminCreateStderr = "Command error: user already exists [name: admin]"
	job := events.NewJob()

	err := Up(context.Background(), job, host, testBundle(t), testOptions("/opt/farrier"))
	if err != nil {
		t.Fatalf("Up: %v, want nil on a host that's already bootstrapped", err)
	}

	evs := drain(job)
	last := evs[len(evs)-1]
	if last.State != events.StateSucceeded || last.Step != "" {
		t.Errorf("last event = %+v, want a job-terminal success", last)
	}
}

// TestUpEmbedsAppINIChecksumSoContentChangesForceRecreate exercises the
// other half of UP-003: `docker compose up -d` recreates a service by
// diffing its resolved config, never the bytes of a file a bind mount
// happens to point at, so a manifest change that only alters app.ini's
// content would otherwise ship to disk without the running container ever
// picking it up. Up carries a checksum of the shipped app.ini as an
// environment variable on the forgejo service precisely so that a
// content-only change is visible to Converge's diff.
func TestUpEmbedsAppINIChecksumSoContentChangesForceRecreate(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()

	if err := Up(context.Background(), job, host, testBundle(t), testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	appINI, ok := host.files["/opt/farrier/forge/app.ini"]
	if !ok {
		t.Fatalf("app.ini not shipped, wrote: %v", keysOf(host.files))
	}
	sum := sha256.Sum256([]byte(appINI))
	wantChecksum := hex.EncodeToString(sum[:])

	composePath := "/opt/farrier/compose.tmp/" + orchestrate.ComposeFile
	compose, ok := host.files[composePath]
	if !ok {
		t.Fatalf("compose file not shipped under %s, wrote: %v", composePath, keysOf(host.files))
	}
	if !strings.Contains(compose, appINIChecksumEnv) || !strings.Contains(compose, wantChecksum) {
		t.Errorf("shipped compose missing %s=%s:\n%s", appINIChecksumEnv, wantChecksum, compose)
	}
}

// UP-002 completes with the forge serving HTTPS at the bundle domain, so
// the last thing Up says about a successful deployment is that endpoint —
// the guarantee stated in the event stream both frontends read, not only in
// this package's doc comment.
func TestUpReportsHTTPSEndpointAtTheDomainOnCompletion(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()

	if err := Up(context.Background(), job, host, testBundle(t), testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	var caddyReady string
	for _, ev := range drain(job) {
		if ev.Step == StepWaitCaddy && ev.State == events.StateSucceeded {
			caddyReady = ev.Detail
		}
	}
	if caddyReady == "" {
		t.Fatal("Up: no succeeded event for the wait-caddy step")
	}
	if !strings.Contains(caddyReady, "https://example.com") {
		t.Errorf("wait-caddy detail = %q, want the https endpoint at the bundle domain", caddyReady)
	}
}

// A nameless bundle (INIT-005) has no domain, so `up` is where the
// instance learns how it is reached (UP-006). Without an address there is
// nothing to render a ROOT_URL or a Caddy site from, so Up refuses rather
// than half-deploying an instance nothing can reach.
func TestUpRejectsNamelessBundleWithoutAnAddress(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()

	err := Up(context.Background(), job, host, namelessBundle(t), testOptions("/opt/farrier"))
	if err == nil {
		t.Fatal("Up: want error for a nameless bundle, got nil")
	}
	if !strings.Contains(err.Error(), "address") {
		t.Errorf("error = %v, want it to name the address it is missing", err)
	}

	// Ahead of CheckHost: a refused deployment leaves the host exactly as
	// it found it, with nothing shipped and nothing run.
	if len(host.commands) != 0 {
		t.Errorf("host commands = %v, want none — Up must refuse before touching the host", host.commands)
	}
	if len(host.files) != 0 {
		t.Errorf("host files = %v, want none — Up must refuse before touching the host", keysOf(host.files))
	}

	evs := drain(job)
	if len(evs) == 0 {
		t.Fatal("Up: emitted no events")
	}
	last := evs[len(evs)-1]
	if last.State != events.StateFailed || last.Step != "" {
		t.Errorf("last event = %+v, want a job-terminal failure", last)
	}
	if !strings.Contains(last.Detail, "address") {
		t.Errorf("terminal event detail = %q, want it to name the address it is missing", last.Detail)
	}
}

func TestUpRejectsNilBundle(t *testing.T) {
	job := events.NewJob()
	if err := Up(context.Background(), job, newFakeHost(), nil, Options{RemoteDir: "/opt/farrier"}); err == nil {
		t.Fatal("Up: want error for nil bundle, got nil")
	}
}

func TestUpRejectsEmptyRemoteDir(t *testing.T) {
	job := events.NewJob()
	if err := Up(context.Background(), job, newFakeHost(), testBundle(t), Options{}); err == nil {
		t.Fatal("Up: want error for empty remote directory, got nil")
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
