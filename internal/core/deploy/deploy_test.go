package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
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

	if strings.Contains(command, "exec -T forgejo true") {
		if f.execFailures > 0 {
			f.execFailures--
			return errors.New("container not ready")
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

func testBundle() *bundle.Bundle {
	return &bundle.Bundle{
		Manifest: *bundle.NewManifest("example.com", map[string]string{
			"forgejo": "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("a", 64),
			"caddy":   "docker.io/library/caddy@sha256:" + strings.Repeat("b", 64),
		}, bundle.DriverConfig{
			Keystore: bundle.DriverRef{Driver: "file", Config: map[string]any{"path": "testdata/keys"}},
			Blob:     bundle.DriverRef{Driver: "local", Config: map[string]any{"path": "testdata/blobs"}},
		}, bundle.ACMEConfig{DNSProvider: "manual", Email: "ops@example.com"}),
		Compose: map[string][]byte{
			"docker-compose.yml": []byte("services:\n  forgejo:\n    image: x\n  caddy:\n    image: y\n"),
		},
	}
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

	err := Up(context.Background(), job, host, testBundle(), Options{RemoteDir: "/opt/farrier", CertIssuer: issuer})
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

	var sawCheckHost, sawComposeUp, sawExecForgejoReady, sawExecCaddyReady, sawAdminCreate bool
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
		case strings.Contains(cmd, "exec -T caddy true"):
			sawExecCaddyReady = true
		case strings.Contains(cmd, "admin user create"):
			sawAdminCreate = true
			if !strings.Contains(cmd, "COMPOSE_PROJECT_NAME=farrier") {
				t.Errorf("admin create command missing project name: %q", cmd)
			}
		}
	}
	if !sawCheckHost || !sawComposeUp || !sawExecForgejoReady || !sawExecCaddyReady || !sawAdminCreate {
		t.Fatalf("missing a step: checkHost=%v composeUp=%v execForgejoReady=%v execCaddyReady=%v adminCreate=%v (commands: %v)",
			sawCheckHost, sawComposeUp, sawExecForgejoReady, sawExecCaddyReady, sawAdminCreate, host.commands)
	}
}

func TestUpPublishesCaddyHTTPSPort(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()

	if err := Up(context.Background(), job, host, testBundle(), testOptions("/opt/farrier")); err != nil {
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

	err := Up(context.Background(), job, host, testBundle(), Options{RemoteDir: "/opt/farrier", CertIssuer: issuer})
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

	if err := Up(context.Background(), job, host, testBundle(), Options{RemoteDir: "/opt/farrier", CertIssuer: issuer}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if len(issuer.calls) != 0 {
		t.Errorf("cert issuer calls = %d, want 0 — a fresh persisted certificate must not be reissued", len(issuer.calls))
	}

	persistedCert, err := os.ReadFile("testdata/keys/tls_certificate")
	if err != nil {
		t.Fatalf("read testdata certificate: %v", err)
	}
	if host.files["/opt/farrier/caddy/tls.crt"] != string(persistedCert) {
		t.Error("shipped certificate does not match the persisted testdata certificate")
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

// TestUpReportsRenewedCertificateNotPersisted exercises the rare branch
// where the persisted certificate is due for renewal: Up must still
// succeed, using the freshly issued certificate for this deploy, and must
// tell the operator through the event stream that the renewal was not
// persisted back to the keystore (persisting it is ACME-002's gap, not
// UP-003's).
func TestUpReportsRenewedCertificateNotPersisted(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()
	issuer := &fakeCertIssuer{}

	if err := Up(context.Background(), job, host, testBundle(), Options{RemoteDir: "/opt/farrier", CertIssuer: issuer}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if host.files["/opt/farrier/caddy/tls.crt"] != "fake-cert-pem" {
		t.Error("Up did not ship the freshly issued certificate for this deploy")
	}

	evs := drain(job)
	var sawNotPersisted bool
	for _, ev := range evs {
		if ev.Step == StepConfigureTLS && ev.State == events.StateSucceeded && strings.Contains(ev.Detail, "not persisted") {
			sawNotPersisted = true
		}
	}
	if !sawNotPersisted {
		t.Errorf("no configure-tls success event reporting the renewal was not persisted, events: %+v", evs)
	}
}

func TestUpRetriesUntilForgejoReady(t *testing.T) {
	host := newFakeHost()
	host.execFailures = 2
	job := events.NewJob()

	if err := Up(context.Background(), job, host, testBundle(), testOptions("/opt/farrier")); err != nil {
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

func TestUpFailsWhenDockerUnreachable(t *testing.T) {
	host := newFakeHost()
	host.checkHostErr = errors.New("no docker")
	job := events.NewJob()

	err := Up(context.Background(), job, host, testBundle(), Options{RemoteDir: "/opt/farrier"})
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

	if err := Up(context.Background(), job, host, testBundle(), testOptions("/opt/farrier")); err == nil {
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

	err := Up(context.Background(), job, host, testBundle(), testOptions("/opt/farrier"))
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

	if err := Up(context.Background(), job, host, testBundle(), testOptions("/opt/farrier")); err != nil {
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

func TestUpRejectsNilBundle(t *testing.T) {
	job := events.NewJob()
	if err := Up(context.Background(), job, newFakeHost(), nil, Options{RemoteDir: "/opt/farrier"}); err == nil {
		t.Fatal("Up: want error for nil bundle, got nil")
	}
}

func TestUpRejectsEmptyRemoteDir(t *testing.T) {
	job := events.NewJob()
	if err := Up(context.Background(), job, newFakeHost(), testBundle(), Options{}); err == nil {
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
