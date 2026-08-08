package status

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/caddy"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// *orchestrate.Client must keep satisfying Runner, since it's the type
// Check runs against in the live CLI path.
var _ Runner = (*orchestrate.Client)(nil)

// fakeRunner plays back a canned Output result per command, keyed by a
// substring match so tests don't have to reproduce ComposeCommand's exact
// prefix.
type fakeRunner struct {
	byContains map[string]fakeResult
	commands   []string
}

type fakeResult struct {
	out []byte
	err error
}

func (f *fakeRunner) Output(ctx context.Context, command string) ([]byte, error) {
	f.commands = append(f.commands, command)
	for substr, res := range f.byContains {
		if strings.Contains(command, substr) {
			return res.out, res.err
		}
	}
	return nil, fmt.Errorf("fakeRunner: unexpected command: %s", command)
}

// fakeKeystore resolves a fixed set of secrets by name.
type fakeKeystore struct {
	secrets map[string]keystore.Secret
	err     error
}

func (f *fakeKeystore) Resolve(ctx context.Context, keyName string) (keystore.Secret, error) {
	if f.err != nil {
		return keystore.Secret{}, f.err
	}
	secret, ok := f.secrets[keyName]
	if !ok {
		return keystore.Secret{}, fmt.Errorf("fakeKeystore: no secret named %s", keyName)
	}
	return secret, nil
}

func testBundle(t *testing.T) *bundle.Bundle {
	t.Helper()
	m := bundle.NewManifest("forge.example.com", map[string]string{
		"forgejo": "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("a", 64),
		"caddy":   "docker.io/library/caddy@sha256:" + strings.Repeat("b", 64),
	}, bundle.DriverConfig{
		Keystore: bundle.DriverRef{Driver: "file", Config: map[string]any{"path": "/keys"}},
		Blob:     bundle.DriverRef{Driver: "local", Config: map[string]any{"path": "/blobs"}},
	}, bundle.ACMEConfig{DNSProvider: "cloudflare"})

	compose, err := orchestrate.Render(m)
	if err != nil {
		t.Fatalf("orchestrate.Render: %v", err)
	}
	return &bundle.Bundle{Manifest: *m, Compose: compose}
}

// testCert generates a self-signed certificate valid from notBefore to
// notAfter, PEM-encoded, for exercising checkTLS without a real ACME
// exchange.
func testCert(t *testing.T, notBefore, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "forge.example.com"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	var buf strings.Builder
	if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("pem encode: %v", err)
	}
	return buf.String()
}

func validOptions(t *testing.T, runner Runner, cert string) Options {
	t.Helper()
	return Options{
		Runner:    runner,
		Bundle:    testBundle(t),
		RemoteDir: "/opt/farrier",
		Keystore: &fakeKeystore{secrets: map[string]keystore.Secret{
			state.KeyTLSCertificate: keystore.NewSecret(cert),
		}},
	}
}

func TestCheckReportsRunningServices(t *testing.T) {
	runner := &fakeRunner{byContains: map[string]fakeResult{
		"docker compose ps": {out: []byte(
			`{"Service":"forgejo","State":"running","Status":"Up 3 hours"}` + "\n" +
				`{"Service":"caddy","State":"running","Status":"Up 3 hours"}`,
		)},
		"df -Pk": {out: []byte("Filesystem 1024-blocks Used Available Capacity Mounted\n" +
			"/dev/sda1 104857600 41943040 62914560 40% /\n")},
	}}
	opts := validOptions(t, runner, testCert(t, time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour)))

	report, err := Check(context.Background(), opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	if len(report.Services) != 2 {
		t.Fatalf("got %d services, want 2: %+v", len(report.Services), report.Services)
	}
	byName := make(map[string]ServiceStatus, len(report.Services))
	for _, s := range report.Services {
		byName[s.Name] = s
	}
	for _, name := range []string{forge.Service, caddy.Service} {
		s, ok := byName[name]
		if !ok {
			t.Fatalf("missing service %s in report: %+v", name, report.Services)
		}
		if !s.Up {
			t.Errorf("service %s: Up = false, want true", name)
		}
		if s.Detail != "Up 3 hours" {
			t.Errorf("service %s: Detail = %q, want %q", name, s.Detail, "Up 3 hours")
		}
	}
}

func TestCheckReportsMissingContainerAsDown(t *testing.T) {
	runner := &fakeRunner{byContains: map[string]fakeResult{
		"docker compose ps": {out: []byte(
			`{"Service":"forgejo","State":"exited","Status":"Exited (1) 5 minutes ago"}`,
		)},
		"df -Pk": {out: []byte("Filesystem 1024-blocks Used Available Capacity Mounted\n" +
			"/dev/sda1 104857600 41943040 62914560 40% /\n")},
	}}
	opts := validOptions(t, runner, testCert(t, time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour)))

	report, err := Check(context.Background(), opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	byName := make(map[string]ServiceStatus, len(report.Services))
	for _, s := range report.Services {
		byName[s.Name] = s
	}
	if s := byName[forge.Service]; s.Up {
		t.Errorf("forgejo: Up = true, want false (exited)")
	}
	caddyStatus, ok := byName[caddy.Service]
	if !ok {
		t.Fatalf("missing caddy in report")
	}
	if caddyStatus.Up {
		t.Errorf("caddy: Up = true, want false (no container)")
	}
	if caddyStatus.Detail != "container not found" {
		t.Errorf("caddy: Detail = %q, want %q", caddyStatus.Detail, "container not found")
	}
}

func TestCheckParsesArrayFormattedComposePS(t *testing.T) {
	runner := &fakeRunner{byContains: map[string]fakeResult{
		"docker compose ps": {out: []byte(
			`[{"Service":"forgejo","State":"running","Status":"Up 1 hour"},` +
				`{"Service":"caddy","State":"running","Status":"Up 1 hour"}]`,
		)},
		"df -Pk": {out: []byte("Filesystem 1024-blocks Used Available Capacity Mounted\n" +
			"/dev/sda1 104857600 41943040 62914560 40% /\n")},
	}}
	opts := validOptions(t, runner, testCert(t, time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour)))

	report, err := Check(context.Background(), opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	for _, s := range report.Services {
		if !s.Up {
			t.Errorf("service %s: Up = false, want true", s.Name)
		}
	}
}

func TestCheckReportsValidTLS(t *testing.T) {
	runner := &fakeRunner{byContains: map[string]fakeResult{
		"docker compose ps": {out: []byte(
			`{"Service":"forgejo","State":"running","Status":"Up"}` + "\n" +
				`{"Service":"caddy","State":"running","Status":"Up"}`,
		)},
		"df -Pk": {out: []byte("Filesystem 1024-blocks Used Available Capacity Mounted\n" +
			"/dev/sda1 104857600 41943040 62914560 40% /\n")},
	}}
	notAfter := time.Now().Add(90 * 24 * time.Hour)
	opts := validOptions(t, runner, testCert(t, time.Now().Add(-time.Hour), notAfter))

	report, err := Check(context.Background(), opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !report.TLS.Valid {
		t.Error("TLS.Valid = false, want true")
	}
	if report.TLS.ExpiringSoon {
		t.Error("TLS.ExpiringSoon = true, want false (90 days out)")
	}
	if diff := report.TLS.NotAfter.Sub(notAfter); diff < -time.Second || diff > time.Second {
		t.Errorf("TLS.NotAfter = %v, want ~%v", report.TLS.NotAfter, notAfter)
	}
}

func TestCheckReportsExpiredTLS(t *testing.T) {
	runner := &fakeRunner{byContains: map[string]fakeResult{
		"docker compose ps": {out: []byte(
			`{"Service":"forgejo","State":"running","Status":"Up"}` + "\n" +
				`{"Service":"caddy","State":"running","Status":"Up"}`,
		)},
		"df -Pk": {out: []byte("Filesystem 1024-blocks Used Available Capacity Mounted\n" +
			"/dev/sda1 104857600 41943040 62914560 40% /\n")},
	}}
	opts := validOptions(t, runner, testCert(t, time.Now().Add(-90*24*time.Hour), time.Now().Add(-time.Hour)))

	report, err := Check(context.Background(), opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.TLS.Valid {
		t.Error("TLS.Valid = true, want false (expired)")
	}
	if !report.TLS.ExpiringSoon {
		t.Error("TLS.ExpiringSoon = false, want true (already past expiry)")
	}
}

func TestCheckReportsExpiringSoonTLS(t *testing.T) {
	runner := &fakeRunner{byContains: map[string]fakeResult{
		"docker compose ps": {out: []byte(
			`{"Service":"forgejo","State":"running","Status":"Up"}` + "\n" +
				`{"Service":"caddy","State":"running","Status":"Up"}`,
		)},
		"df -Pk": {out: []byte("Filesystem 1024-blocks Used Available Capacity Mounted\n" +
			"/dev/sda1 104857600 41943040 62914560 40% /\n")},
	}}
	opts := validOptions(t, runner, testCert(t, time.Now().Add(-time.Hour), time.Now().Add(10*24*time.Hour)))

	report, err := Check(context.Background(), opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !report.TLS.Valid {
		t.Error("TLS.Valid = false, want true")
	}
	if !report.TLS.ExpiringSoon {
		t.Error("TLS.ExpiringSoon = false, want true (10 days out, inside the 14-day window)")
	}
}

func TestCheckReportsDiskHeadroom(t *testing.T) {
	runner := &fakeRunner{byContains: map[string]fakeResult{
		"docker compose ps": {out: []byte(
			`{"Service":"forgejo","State":"running","Status":"Up"}` + "\n" +
				`{"Service":"caddy","State":"running","Status":"Up"}`,
		)},
		"df -Pk": {out: []byte("Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
			"/dev/sda1 104857600 41943040 62914560 40% /\n")},
	}}
	opts := validOptions(t, runner, testCert(t, time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour)))

	report, err := Check(context.Background(), opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Disk.Path != DefaultDiskPath {
		t.Errorf("Disk.Path = %q, want %q", report.Disk.Path, DefaultDiskPath)
	}
	if report.Disk.TotalBytes != 104857600*1024 {
		t.Errorf("Disk.TotalBytes = %d, want %d", report.Disk.TotalBytes, uint64(104857600*1024))
	}
	if report.Disk.UsedBytes != 41943040*1024 {
		t.Errorf("Disk.UsedBytes = %d, want %d", report.Disk.UsedBytes, uint64(41943040*1024))
	}
	if report.Disk.AvailableBytes != 62914560*1024 {
		t.Errorf("Disk.AvailableBytes = %d, want %d", report.Disk.AvailableBytes, uint64(62914560*1024))
	}
}

func TestCheckUsesCustomDiskPathAndServices(t *testing.T) {
	runner := &fakeRunner{byContains: map[string]fakeResult{
		"docker compose ps": {out: []byte(
			`{"Service":"forgejo","State":"running","Status":"Up"}`,
		)},
		"df -Pk '/data'": {out: []byte("Filesystem 1024-blocks Used Available Capacity Mounted\n" +
			"/dev/sdb1 100 50 50 50% /data\n")},
	}}
	opts := validOptions(t, runner, testCert(t, time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour)))
	opts.DiskPath = "/data"
	opts.Services = []string{forge.Service}

	report, err := Check(context.Background(), opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Services) != 1 || report.Services[0].Name != forge.Service {
		t.Errorf("Services = %+v, want just %s", report.Services, forge.Service)
	}
	if report.Disk.Path != "/data" {
		t.Errorf("Disk.Path = %q, want %q", report.Disk.Path, "/data")
	}
}

func TestCheckRequiresRunner(t *testing.T) {
	opts := validOptions(t, nil, testCert(t, time.Now(), time.Now().Add(time.Hour)))
	opts.Runner = nil
	if _, err := Check(context.Background(), opts); err == nil {
		t.Fatal("Check succeeded without a runner, want error")
	}
}

func TestCheckRequiresBundle(t *testing.T) {
	opts := validOptions(t, &fakeRunner{}, testCert(t, time.Now(), time.Now().Add(time.Hour)))
	opts.Bundle = nil
	if _, err := Check(context.Background(), opts); err == nil {
		t.Fatal("Check succeeded without a bundle, want error")
	}
}

func TestCheckRequiresKeystore(t *testing.T) {
	opts := validOptions(t, &fakeRunner{}, testCert(t, time.Now(), time.Now().Add(time.Hour)))
	opts.Keystore = nil
	if _, err := Check(context.Background(), opts); err == nil {
		t.Fatal("Check succeeded without a keystore, want error")
	}
}

func TestCheckWrapsServicesFailure(t *testing.T) {
	runner := &fakeRunner{byContains: map[string]fakeResult{
		"docker compose ps": {err: errors.New("ssh: session failed")},
	}}
	opts := validOptions(t, runner, testCert(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))

	_, err := Check(context.Background(), opts)
	if err == nil {
		t.Fatal("Check succeeded, want error")
	}
	if !strings.Contains(err.Error(), "services") {
		t.Errorf("error = %v, want it to name the services check", err)
	}
}

func TestCheckWrapsTLSFailure(t *testing.T) {
	runner := &fakeRunner{byContains: map[string]fakeResult{
		"docker compose ps": {out: []byte(
			`{"Service":"forgejo","State":"running","Status":"Up"}` + "\n" +
				`{"Service":"caddy","State":"running","Status":"Up"}`,
		)},
	}}
	opts := Options{
		Runner:    runner,
		Bundle:    testBundle(t),
		RemoteDir: "/opt/farrier",
		Keystore:  &fakeKeystore{err: errors.New("keystore unreachable")},
	}

	_, err := Check(context.Background(), opts)
	if err == nil {
		t.Fatal("Check succeeded, want error")
	}
	if !strings.Contains(err.Error(), "tls") {
		t.Errorf("error = %v, want it to name the tls check", err)
	}
}

func TestCheckWrapsDiskFailure(t *testing.T) {
	runner := &fakeRunner{byContains: map[string]fakeResult{
		"docker compose ps": {out: []byte(
			`{"Service":"forgejo","State":"running","Status":"Up"}` + "\n" +
				`{"Service":"caddy","State":"running","Status":"Up"}`,
		)},
		"df -Pk": {err: errors.New("no such file or directory")},
	}}
	opts := validOptions(t, runner, testCert(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))

	_, err := Check(context.Background(), opts)
	if err == nil {
		t.Fatal("Check succeeded, want error")
	}
	if !strings.Contains(err.Error(), "disk") {
		t.Errorf("error = %v, want it to name the disk check", err)
	}
}

func TestParseComposePSRejectsGarbage(t *testing.T) {
	if _, err := parseComposePS([]byte("not json")); err == nil {
		t.Fatal("parseComposePS succeeded on invalid input, want error")
	}
}

func TestParseComposePSHandlesEmptyOutput(t *testing.T) {
	got, err := parseComposePS([]byte(""))
	if err != nil {
		t.Fatalf("parseComposePS: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries, want 0", len(got))
	}
}

func TestParseDFRejectsShortOutput(t *testing.T) {
	if _, err := parseDF([]byte("just a header\n"), "/"); err == nil {
		t.Fatal("parseDF succeeded on missing data line, want error")
	}
}

func TestParseDFRejectsMalformedFields(t *testing.T) {
	if _, err := parseDF([]byte("Filesystem 1024-blocks Used Available Capacity Mounted\nnot-enough-fields\n"), "/"); err == nil {
		t.Fatal("parseDF succeeded on malformed data line, want error")
	}
}
