package attach

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
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/acme"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/caddy"
	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/state"
	"gopkg.in/yaml.v3"
)

// These tests are UP-007's acceptance bar, driven end to end through Attach
// against a fake host: a nameless instance gains a name in place — the zone
// is proven, a certificate is issued and persisted, every piece of
// configuration that derives from the name is re-rendered, and the clone
// URLs that changed are reported — while nothing that makes the instance
// *this* instance moves.

const (
	newDomain  = "forge.example.com"
	oldAddress = "box.tail1234.ts.net"
	remoteDir  = "/opt/farrier"
)

// ---------------------------------------------------------------- fakes

// fakeHost implements Host without a real SSH server. It is deliberately
// permissive: every step of deploy.Up has to succeed for Attach's own
// sequencing to be observable, and what these tests assert on is the files
// it was handed and the Compose it was converged to.
type fakeHost struct {
	mu sync.Mutex

	files    map[string]string
	commands []string

	// failCommand makes every command containing it fail, so a test can
	// break the deployment after the bundle has already been named.
	failCommand string
}

func newFakeHost() *fakeHost {
	return &fakeHost{files: make(map[string]string)}
}

func (f *fakeHost) record(command string) error {
	f.commands = append(f.commands, command)
	if f.failCommand != "" && strings.Contains(command, f.failCommand) {
		return fmt.Errorf("fakeHost: %s failed", f.failCommand)
	}
	return nil
}

func (f *fakeHost) Output(ctx context.Context, command string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(command); err != nil {
		return nil, err
	}
	// deploy.ReadStateVersion's read of the recorded forge version: serve
	// it out of the same map WriteFile stores into, so the fake's reads and
	// writes agree the way a real host's do. Without it every Up here would
	// see an empty record.
	if rest, ok := strings.CutPrefix(command, "if [ -f '"); ok {
		p, _, _ := strings.Cut(rest, "'")
		return []byte(f.files[p]), nil
	}
	return nil, nil
}

func (f *fakeHost) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = string(content)
	return nil
}

func (f *fakeHost) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.record(command)
}

func (f *fakeHost) CheckHost(ctx context.Context) error { return nil }
func (f *fakeHost) Close() error                        { return nil }

func (f *fakeHost) wrote(path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.files[path]
	return content, ok
}

// fakeProver stands in for a real ACME DNS-01 exchange, recording what it
// was asked to prove and handing back a self-signed certificate for it.
type fakeProver struct {
	calls []string
	err   error
	cert  *acme.Certificate
}

func (p *fakeProver) Prove(domain, dnsProvider, email string) (*acme.Certificate, error) {
	p.calls = append(p.calls, fmt.Sprintf("%s|%s|%s", domain, dnsProvider, email))
	if p.err != nil {
		return nil, p.err
	}
	if p.cert != nil {
		return p.cert, nil
	}
	return selfSigned(domain)
}

// reuseIssuer is the CertIssuer deploy.Up is handed: it asserts the
// certificate Attach persisted is already there and fresh, by refusing to
// issue a new one. A run that reached the ACME server here would be one
// where the persist step did not take.
type reuseIssuer struct {
	existingSeen []*acme.Certificate
}

func (r *reuseIssuer) EnsureValid(cfg acme.Config, existing *acme.Certificate, now time.Time) (*acme.Certificate, bool, error) {
	r.existingSeen = append(r.existingSeen, existing)
	if existing == nil {
		return nil, false, errors.New("reuseIssuer: no persisted certificate to reuse")
	}
	return existing, false, nil
}

// selfSigned builds a certificate valid for domain, PEM-encoded the way
// acme.Issue returns one so acme.ParseCertificate can read it back.
func selfSigned(domain string) (*acme.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &acme.Certificate{
		Domain:      domain,
		Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		PrivateKey:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// ------------------------------------------------------------- fixtures

// namelessBundleDir writes a nameless bundle (INIT-005) to a fresh
// directory with a file keystore holding everything init generates for one
// — every piece of key material except the two TLS entries, which a
// nameless bundle has no name to issue.
func namelessBundleDir(t *testing.T) (dir string, keysDir string) {
	t.Helper()
	dir = t.TempDir()
	keysDir = t.TempDir()

	for name, content := range map[string]string{
		forge.KeySecretKey:        "secret-key",
		forge.KeyInternalToken:    "internal-token",
		forge.KeyLFSJWTSecret:     "lfs-jwt-secret",
		state.KeySSHHostKey:       sshHostKeyPEM,
		state.KeySSHHostKeyPublic: "ssh-ed25519 AAAA host\n",
	} {
		if err := os.WriteFile(filepath.Join(keysDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	m := bundle.NewManifest("", map[string]string{
		"forgejo": "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("a", 64),
		"caddy":   "docker.io/library/caddy@sha256:" + strings.Repeat("b", 64),
	}, bundle.DriverConfig{
		Keystore: bundle.DriverRef{Driver: "file", Config: map[string]any{"path": keysDir}},
		Blob:     bundle.DriverRef{Driver: "local", Config: map[string]any{"path": t.TempDir()}},
	}, bundle.ACMEConfig{})
	m.GitSSHPort = bundle.DefaultGitSSHPort
	// The manifest's copy of the host public key, as `init` writes it: the
	// same key the keystore holds, minus the trailing newline.
	m.SSHHostKeyPublic = "ssh-ed25519 AAAA host"

	compose, err := orchestrate.Render(m)
	if err != nil {
		t.Fatalf("render compose: %v", err)
	}
	b := &bundle.Bundle{Manifest: *m, Compose: compose}
	if err := b.Save(dir); err != nil {
		t.Fatalf("save nameless bundle: %v", err)
	}
	return dir, keysDir
}

// sshHostKeyPEM is a fixed ed25519 host key in OpenSSH PEM format. Its
// bytes never matter to these tests — what matters is that the same bytes
// come back out of the keystore and onto the host after the instance is
// named (RSTR-004).
const sshHostKeyPEM = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACBHVMFFY0nJXBLXVGDVzcaqzUvlz7gCVZ0DACeJDKZTGwAAAJDGDs1Nxg7N
TQAAAAtzc2gtZWQyNTUxOQAAACBHVMFFY0nJXBLXVGDVzcaqzUvlz7gCVZ0DACeJDKZTGw
AAAEBQPmhLBBFCLhLzXhx7DWyxJvj7yYbXKMHVJqvIYCGQIkdUwUVjSclcEtdUYNXNxqrN
S+XPuAJVnQMAJ4kMplMbAAAAC2ZhcnJpZXJAdGVzdAE=
-----END OPENSSH PRIVATE KEY-----
`

// testOptions is a complete, valid Attach for the bundle at dir.
func testOptions(t *testing.T, dir string, host Host) Options {
	t.Helper()
	b, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	return Options{
		BundleDir:       dir,
		RemoteDir:       remoteDir,
		Bundle:          b,
		Host:            host,
		Domain:          newDomain,
		ACMEDNSProvider: "cloudflare",
		ACMEEmail:       "ops@example.com",
		Address:         oldAddress,
		Prover:          &fakeProver{},
		CertIssuer:      &reuseIssuer{},
	}
}

// details collects every event detail on a finished job, so a test can
// assert on what the operator was told (CORE-002).
func details(t *testing.T, job *events.Job, run func() error) ([]string, error) {
	t.Helper()
	stream, cancel := job.Subscribe()
	defer cancel()

	var (
		wg   sync.WaitGroup
		seen []string
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ev := range stream {
			seen = append(seen, ev.Detail)
		}
	}()
	err := run()
	wg.Wait()
	return seen, err
}

func containing(details []string, want string) bool {
	for _, d := range details {
		if strings.Contains(d, want) {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------- the happy path

// UP-007's core claim, end to end: the instance is named in place. The
// manifest gains the domain, the certificate is persisted as bundle key
// material, and the host is re-rendered to serve HTTPS at the new name.
func TestAttachNamesANamelessInstanceInPlace(t *testing.T) {
	dir, keysDir := namelessBundleDir(t)
	host := newFakeHost()
	opts := testOptions(t, dir, host)

	named, err := Attach(context.Background(), events.NewJob(), opts)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	if named.Manifest.Domain != newDomain {
		t.Errorf("returned manifest domain = %q, want %q", named.Manifest.Domain, newDomain)
	}
	if !named.Manifest.Named() {
		t.Error("returned manifest does not report itself named")
	}
	if named.Manifest.ACME.DNSProvider != "cloudflare" || named.Manifest.ACME.Email != "ops@example.com" {
		t.Errorf("acme section = %+v, want the provider and email attach was given", named.Manifest.ACME)
	}

	// The bundle on disk carries the name, not just the value returned.
	reloaded, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("reload bundle: %v", err)
	}
	if reloaded.Manifest.Domain != newDomain {
		t.Errorf("saved manifest domain = %q, want %q", reloaded.Manifest.Domain, newDomain)
	}

	// The certificate is in the keystore under the names everything else
	// reads it by.
	for _, name := range []string{state.KeyTLSCertificate, state.KeyTLSPrivateKey} {
		raw, err := os.ReadFile(filepath.Join(keysDir, name))
		if err != nil {
			t.Fatalf("read persisted %s: %v", name, err)
		}
		if len(raw) == 0 {
			t.Errorf("persisted %s is empty", name)
		}
	}

	// The host now serves HTTPS at the new name.
	caddyfile, ok := host.wrote(remoteDir + "/caddy/Caddyfile")
	if !ok {
		t.Fatal("no Caddyfile shipped")
	}
	if !strings.Contains(caddyfile, newDomain) {
		t.Errorf("Caddyfile does not serve %s:\n%s", newDomain, caddyfile)
	}
	if strings.Contains(caddyfile, "http://"+oldAddress) {
		t.Errorf("Caddyfile still serves the old address:\n%s", caddyfile)
	}
	if _, ok := host.wrote(remoteDir + "/caddy/tls.crt"); !ok {
		t.Error("no certificate shipped to the host; a named instance terminates TLS")
	}

	appINI, ok := host.wrote(remoteDir + "/forge/app.ini")
	if !ok {
		t.Fatal("no app.ini shipped")
	}
	for _, want := range []string{
		"ROOT_URL = https://" + newDomain + "/",
		"DOMAIN = " + newDomain,
		"SSH_DOMAIN = " + newDomain,
	} {
		if !strings.Contains(appINI, want) {
			t.Errorf("app.ini missing %q:\n%s", want, appINI)
		}
	}
	if strings.Contains(appINI, oldAddress) {
		t.Errorf("app.ini still carries the old address %s:\n%s", oldAddress, appINI)
	}
}

// The zone is proven through the provider the operator named, for the
// domain they named — and the certificate that exchange produced is the one
// deploy.Up serves, rather than a second one issued for the same name.
func TestAttachProvesTheZoneAndReusesThatCertificate(t *testing.T) {
	dir, _ := namelessBundleDir(t)
	prover := &fakeProver{}
	issuer := &reuseIssuer{}

	opts := testOptions(t, dir, newFakeHost())
	opts.Prover = prover
	opts.CertIssuer = issuer

	if _, err := Attach(context.Background(), events.NewJob(), opts); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	if len(prover.calls) != 1 {
		t.Fatalf("prover called %d times, want exactly 1", len(prover.calls))
	}
	if want := newDomain + "|cloudflare|ops@example.com"; prover.calls[0] != want {
		t.Errorf("proved %q, want %q", prover.calls[0], want)
	}
	// reuseIssuer errors unless it is handed an existing certificate, so
	// reaching here at all means the persisted one was found. Assert the
	// shape explicitly so a future change to the issuer cannot hide it.
	if len(issuer.existingSeen) != 1 || issuer.existingSeen[0] == nil {
		t.Fatalf("deploy.Up saw existing certificates %v, want exactly one non-nil", issuer.existingSeen)
	}
	if issuer.existingSeen[0].Domain != newDomain {
		t.Errorf("certificate handed to deploy.Up is for %q, want %q", issuer.existingSeen[0].Domain, newDomain)
	}
}

// ----------------------------------------------- what must not change

// The promise spec.md "Instances without a name" makes, at the level this
// package can enforce it: naming an instance touches the name and nothing
// else. Everything the operator would lose in a rebuild is host state under
// RemoteDir/state or bundle key material, and neither moves.
func TestAttachPreservesTheInstance(t *testing.T) {
	dir, keysDir := namelessBundleDir(t)

	before, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	keysBefore := snapshotDir(t, keysDir)

	host := newFakeHost()
	named, err := Attach(context.Background(), events.NewJob(), testOptions(t, dir, host))
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// The SSH host key is non-rotating bundle key material (RSTR-004):
	// clients that already accepted it must not see a mismatch.
	keysAfter := snapshotDir(t, keysDir)
	for _, name := range []string{state.KeySSHHostKey, state.KeySSHHostKeyPublic} {
		if keysAfter[name] != keysBefore[name] {
			t.Errorf("%s changed; the SSH host key must survive naming (RSTR-004)", name)
		}
	}
	// So are Forgejo's own secrets — the sessions, tokens, and LFS links
	// they underwrite are exactly the "secrets" UP-007 says survive.
	for _, name := range []string{forge.KeySecretKey, forge.KeyInternalToken, forge.KeyLFSJWTSecret} {
		if keysAfter[name] != keysBefore[name] {
			t.Errorf("%s changed; forge secrets must survive naming", name)
		}
	}
	// The only key material that appears is the certificate the new name
	// needs. Nothing is removed.
	for name := range keysBefore {
		if _, ok := keysAfter[name]; !ok {
			t.Errorf("%s disappeared from the keystore", name)
		}
	}
	for name := range keysAfter {
		if _, ok := keysBefore[name]; ok {
			continue
		}
		if name != state.KeyTLSCertificate && name != state.KeyTLSPrivateKey {
			t.Errorf("unexpected new key material %q; naming issues a certificate and nothing else", name)
		}
	}

	// Everything about the manifest except the name and the ACME section it
	// pairs with is carried through untouched: the pinned digests a restore
	// depends on, the git-over-SSH port existing remotes use, and the driver
	// targets key material and blobs live behind.
	if named.Manifest.GitSSHPort != before.Manifest.GitSSHPort {
		t.Errorf("git-over-ssh port moved: %d -> %d", before.Manifest.GitSSHPort, named.Manifest.GitSSHPort)
	}
	// Naming an instance changes its domain, not its host key. The
	// manifest's copy of that key is what `publish` pins the endpoint
	// against, so a stale one would break every push after an attach with a
	// fingerprint mismatch — against a key the instance never stopped
	// presenting.
	if named.Manifest.SSHHostKeyPublic != before.Manifest.SSHHostKeyPublic {
		t.Errorf("manifest ssh host public key changed: %q -> %q", before.Manifest.SSHHostKeyPublic, named.Manifest.SSHHostKeyPublic)
	}
	if want := strings.TrimSpace(keysAfter[state.KeySSHHostKeyPublic]); named.Manifest.SSHHostKeyPublic != want {
		t.Errorf("manifest ssh host public key = %q, want the key the keystore still holds, %q", named.Manifest.SSHHostKeyPublic, want)
	}
	for component, image := range before.Manifest.Images {
		if named.Manifest.Images[component] != image {
			t.Errorf("image %q re-pinned: %q -> %q", component, image, named.Manifest.Images[component])
		}
	}
	if named.Manifest.Drivers.Keystore.Driver != before.Manifest.Drivers.Keystore.Driver {
		t.Error("keystore driver changed")
	}
	if len(named.Manifest.State) != len(before.Manifest.State) {
		t.Error("state declarations changed; the four-kind state model is not the name's to touch")
	}

	// The host state directories are the ones the instance is already
	// running from — deploy.Up creates them idempotently rather than
	// replacing them, which is why the git data, database, and LFS objects
	// under them survive (UP-004).
	if _, ok := host.wrote(remoteDir + "/forge/app.ini"); !ok {
		t.Fatal("no app.ini shipped")
	}
	joined := strings.Join(host.commands, "\n")
	for _, forbidden := range []string{"rm -rf " + remoteDir + "/state", "docker compose down -v", "docker volume rm"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("attach ran %q; naming an instance must not destroy its state", forbidden)
		}
	}

	// The same persisted host key is reinstalled, byte for byte, at the
	// path deploy configures Forgejo to load it from.
	keyPath := filepath.Join(deploy.GiteaStatePath(remoteDir), strings.TrimPrefix(forge.SSHHostKeyPath, forge.DataPath+"/"))
	installed, ok := host.wrote(keyPath)
	if !ok {
		t.Fatalf("no SSH host key installed at %s", keyPath)
	}
	if installed != sshHostKeyPEM {
		t.Error("the SSH host key installed on the host is not the bundle's persisted one (RSTR-004)")
	}
}

// convergedCompose decodes the Compose definition Attach's deployment
// shipped to the host, so a test can assert on what the instance actually
// converged to.
func convergedCompose(t *testing.T, host *fakeHost) map[string]any {
	t.Helper()
	raw, ok := host.wrote(filepath.Join(remoteDir, "compose.tmp", orchestrate.ComposeFile))
	if !ok {
		t.Fatalf("no compose file shipped; host holds %v", hostPaths(host))
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decode shipped compose: %v", err)
	}
	services, _ := doc["services"].(map[string]any)
	return services
}

func hostPaths(host *fakeHost) []string {
	host.mu.Lock()
	defer host.mu.Unlock()
	paths := make([]string, 0, len(host.files))
	for p := range host.files {
		paths = append(paths, p)
	}
	return paths
}

// publishedPorts returns the host:container port mappings the converged
// definition publishes on service.
func publishedPorts(t *testing.T, services map[string]any, service string) []string {
	t.Helper()
	svc, ok := services[service].(map[string]any)
	if !ok {
		t.Fatalf("converged compose declares no %q service", service)
	}
	raw, _ := svc["ports"].([]any)
	ports := make([]string, 0, len(raw))
	for _, p := range raw {
		ports = append(ports, fmt.Sprint(p))
	}
	return ports
}

// snapshotDir reads every file directly under dir into a name -> content
// map.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		out[entry.Name()] = string(raw)
	}
	return out
}

// The endpoint moves from plain HTTP on 80 to HTTPS on 443, and the
// git-over-SSH port does not move at all — the half of the change that
// makes existing SSH remotes need only a hostname edit.
func TestAttachMovesTheWebEndpointAndLeavesGitSSHAlone(t *testing.T) {
	dir, _ := namelessBundleDir(t)
	host := newFakeHost()

	if _, err := Attach(context.Background(), events.NewJob(), testOptions(t, dir, host)); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	converged := convergedCompose(t, host)
	caddyPorts := publishedPorts(t, converged, caddy.Service)
	if !hasPort(caddyPorts, "443:443") {
		t.Errorf("caddy ports = %v, want HTTPS published", caddyPorts)
	}
	if hasPort(caddyPorts, "80:80") {
		t.Errorf("caddy still publishes plain HTTP on 80: %v", caddyPorts)
	}

	forgePorts := publishedPorts(t, converged, forge.Service)
	want := fmt.Sprintf("%d:%d", bundle.DefaultGitSSHPort, forge.SSHListenPort)
	if !hasPort(forgePorts, want) {
		t.Errorf("forgejo ports = %v, want git over SSH still on %s", forgePorts, want)
	}
}

func hasPort(ports []string, want string) bool {
	for _, p := range ports {
		if p == want {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------- reporting

// UP-007's "report the clone URLs that changed": both halves of every URL
// that moved, on the shared event stream, plus what an operator has to know
// about the host key on the other side of the change.
func TestAttachReportsTheCloneURLsThatChanged(t *testing.T) {
	dir, _ := namelessBundleDir(t)
	job := events.NewJob()
	opts := testOptions(t, dir, newFakeHost())

	seen, err := details(t, job, func() error {
		_, err := Attach(context.Background(), job, opts)
		return err
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	for _, want := range []string{
		// The "was" half carries the nameless tier's published port; the
		// "now" half carries none, because 443 is what https already
		// means (bundle.Manifest.WebURL).
		fmt.Sprintf("http://%s:%d/", oldAddress, bundle.DefaultNamelessWebPort),
		"https://" + newDomain + "/",
		fmt.Sprintf("ssh://git@%s:%d/<owner>/<repo>.git", oldAddress, bundle.DefaultGitSSHPort),
		fmt.Sprintf("ssh://git@%s:%d/<owner>/<repo>.git", newDomain, bundle.DefaultGitSSHPort),
		"git remote set-url origin",
		"SSH host key is unchanged",
	} {
		if !containing(seen, want) {
			t.Errorf("event stream never mentioned %q; details were:\n%s", want, strings.Join(seen, "\n"))
		}
	}
}

// Key material never reaches the event stream (KEY-003) — not the
// certificate the operation just issued, and not the private key beside it.
func TestAttachNeverReportsKeyMaterial(t *testing.T) {
	dir, _ := namelessBundleDir(t)
	cert, err := selfSigned(newDomain)
	if err != nil {
		t.Fatalf("build certificate: %v", err)
	}

	job := events.NewJob()
	opts := testOptions(t, dir, newFakeHost())
	opts.Prover = &fakeProver{cert: cert}

	seen, err := details(t, job, func() error {
		_, err := Attach(context.Background(), job, opts)
		return err
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	joined := strings.Join(seen, "\n")
	for _, secret := range []string{
		strings.TrimSpace(string(cert.PrivateKey)),
		strings.TrimSpace(string(cert.Certificate)),
		sshHostKeyPEM,
		"PRIVATE KEY",
	} {
		if strings.Contains(joined, secret) {
			t.Errorf("event stream carries key material (KEY-003)")
		}
	}
	// The key *names* are reported, which is what tells the operator where
	// the material landed — the same split INIT-006 draws.
	if !containing(seen, state.KeyTLSCertificate) {
		t.Error("event stream never named the certificate it stored")
	}
}

// ------------------------------------------------------------- refusals

// Renaming a named instance is a different operation with different
// consequences, and Attach is not it. See the package doc.
func TestAttachRefusesAnAlreadyNamedBundle(t *testing.T) {
	dir, _ := namelessBundleDir(t)
	host := newFakeHost()
	opts := testOptions(t, dir, host)
	opts.Bundle.Manifest.Domain = "already.example.com"
	opts.Bundle.Manifest.ACME = bundle.ACMEConfig{DNSProvider: "manual"}

	_, err := Attach(context.Background(), events.NewJob(), opts)
	if err == nil {
		t.Fatal("Attach accepted an already-named bundle")
	}
	if !strings.Contains(err.Error(), "already named already.example.com") {
		t.Errorf("error does not name the domain the bundle carries: %v", err)
	}
	if len(host.commands) != 0 {
		t.Errorf("refusal still touched the host: %v", host.commands)
	}
	// The bundle on disk is untouched.
	reloaded, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("reload bundle: %v", err)
	}
	if reloaded.Manifest.Named() {
		t.Error("a refused attach wrote a name into the manifest")
	}
}

func TestAttachRefusesBadInput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Options)
		want   string
	}{
		{"no domain", func(o *Options) { o.Domain = "" }, "domain is required"},
		{"not an fqdn", func(o *Options) { o.Domain = "forge" }, "not a valid DNS name"},
		{"no dns provider", func(o *Options) { o.ACMEDNSProvider = "" }, "dns-01 provider is required"},
		{"no address", func(o *Options) { o.Address = "" }, "currently served at is required"},
		{"address with a scheme", func(o *Options) { o.Address = "http://box" }, "carries a scheme"},
		{"no remote dir", func(o *Options) { o.RemoteDir = "" }, "remote directory is required"},
		{"no bundle dir", func(o *Options) { o.BundleDir = "" }, "bundle directory is required"},
		{"no host", func(o *Options) { o.Host = nil }, "host is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, _ := namelessBundleDir(t)
			host := newFakeHost()
			prover := &fakeProver{}
			opts := testOptions(t, dir, host)
			opts.Prover = prover
			tt.mutate(&opts)

			_, err := Attach(context.Background(), events.NewJob(), opts)
			if err == nil {
				t.Fatalf("Attach accepted %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
			// Validation runs before anything is spent: no ACME exchange,
			// no host contact.
			if len(prover.calls) != 0 {
				t.Errorf("a refused attach spent an ACME exchange: %v", prover.calls)
			}
			if len(host.commands) != 0 {
				t.Errorf("a refused attach touched the host: %v", host.commands)
			}
		})
	}
}

// A nameless bundle holds no TLS key material. Finding some means the
// manifest and the keystore disagree about which instance this is, and
// overwriting it would replace a live certificate issued for another name.
func TestAttachRefusesWhenTheKeystoreAlreadyHoldsACertificate(t *testing.T) {
	dir, keysDir := namelessBundleDir(t)
	if err := os.WriteFile(filepath.Join(keysDir, state.KeyTLSCertificate), []byte("existing"), 0o600); err != nil {
		t.Fatalf("seed certificate: %v", err)
	}

	prover := &fakeProver{}
	opts := testOptions(t, dir, newFakeHost())
	opts.Prover = prover

	_, err := Attach(context.Background(), events.NewJob(), opts)
	if err == nil {
		t.Fatal("Attach accepted a bundle whose keystore already holds a certificate")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("error = %v, want a refusal to overwrite", err)
	}
	if len(prover.calls) != 0 {
		t.Errorf("refused after spending an ACME exchange: %v", prover.calls)
	}
}

// A failed zone proof leaves everything exactly as it was: no certificate,
// no name in the manifest, nothing shipped.
func TestAttachLeavesEverythingAloneWhenTheZoneProofFails(t *testing.T) {
	dir, keysDir := namelessBundleDir(t)
	host := newFakeHost()
	opts := testOptions(t, dir, host)
	opts.Prover = &fakeProver{err: errors.New("no TXT record")}

	_, err := Attach(context.Background(), events.NewJob(), opts)
	if err == nil {
		t.Fatal("Attach succeeded with a failed zone proof")
	}
	if !strings.Contains(err.Error(), "no TXT record") {
		t.Errorf("error does not carry the reason: %v", err)
	}

	reloaded, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("reload bundle: %v", err)
	}
	if reloaded.Manifest.Named() {
		t.Error("the manifest gained a name despite a failed proof")
	}
	if _, err := os.Stat(filepath.Join(keysDir, state.KeyTLSCertificate)); err == nil {
		t.Error("a certificate was persisted despite a failed proof")
	}
	if len(host.commands) != 0 {
		t.Errorf("the host was touched despite a failed proof: %v", host.commands)
	}
}

// A deployment that fails after the manifest is written leaves a named
// bundle and a host that has not converged. The failure has to say so, and
// point at the command that finishes the job — re-running Attach would
// refuse, since the bundle it would name now has a name.
func TestAttachFailedDeploymentPointsAtUp(t *testing.T) {
	dir, _ := namelessBundleDir(t)
	host := newFakeHost()
	host.failCommand = "docker compose up -d"

	opts := testOptions(t, dir, host)
	_, err := Attach(context.Background(), events.NewJob(), opts)
	if err == nil {
		t.Fatal("Attach succeeded against a host that refused to converge")
	}
	if !strings.Contains(err.Error(), "re-run `up`") {
		t.Errorf("error does not point at the recovery command: %v", err)
	}

	reloaded, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("reload bundle: %v", err)
	}
	if reloaded.Manifest.Domain != newDomain {
		t.Errorf("saved manifest domain = %q; the recovery advice assumes the name is already written", reloaded.Manifest.Domain)
	}

	// And a second attach against that bundle is refused rather than
	// half-repeating itself.
	second := testOptions(t, dir, newFakeHost())
	if _, err := Attach(context.Background(), events.NewJob(), second); err == nil {
		t.Error("a second attach against the now-named bundle was accepted")
	}
}

// The job carries a terminal event on both paths, so a frontend rendering
// the stream never leaves a spinner running (CORE-002).
func TestAttachAlwaysTerminatesTheJob(t *testing.T) {
	for _, tt := range []struct {
		name    string
		prepare func(*Options)
		wantErr bool
	}{
		{"success", func(*Options) {}, false},
		{"failure", func(o *Options) { o.Prover = &fakeProver{err: errors.New("nope")} }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir, _ := namelessBundleDir(t)
			job := events.NewJob()
			opts := testOptions(t, dir, newFakeHost())
			tt.prepare(&opts)

			stream, cancel := job.Subscribe()
			defer cancel()

			var (
				wg    sync.WaitGroup
				final events.State
			)
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ev := range stream {
					if ev.Step == "" {
						final = ev.State
					}
				}
			}()

			_, err := Attach(context.Background(), job, opts)
			wg.Wait()

			if tt.wantErr && err == nil {
				t.Fatal("want an error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Attach: %v", err)
			}
			want := events.StateSucceeded
			if tt.wantErr {
				want = events.StateFailed
			}
			if final != want {
				t.Errorf("terminal event state = %q, want %q", final, want)
			}
		})
	}
}
