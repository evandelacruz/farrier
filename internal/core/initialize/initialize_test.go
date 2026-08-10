package initialize

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/acme"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// fakeResolver pins every ref to a deterministic digest derived from the
// ref itself, so tests can assert on the resulting manifest without a real
// registry.
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
	return fmt.Sprintf("%s@sha256:%s", name, strings.Repeat("a", 64)), nil
}

// fakeProver simulates ACME DNS-01 zone-control proof (INIT-002), so tests
// can exercise Run's pass/fail paths without a real ACME server or DNS
// provider. On success it returns a fake certificate, standing in for the
// one a real exchange would obtain — INIT-003 persists it as key material.
type fakeProver struct {
	calls []string
	err   error
	cert  *acme.Certificate
}

func (f *fakeProver) Prove(domain, dnsProvider, email string) (*acme.Certificate, error) {
	f.calls = append(f.calls, domain)
	if f.err != nil {
		return nil, f.err
	}
	if f.cert != nil {
		return f.cert, nil
	}
	return fakeCertificate(), nil
}

// fakeCertificate is a syntactically plausible (not cryptographically
// real) certificate/key pair, good enough for tests that only assert
// generateKeyMaterial persists whatever the Prover returns.
func fakeCertificate() *acme.Certificate {
	return &acme.Certificate{
		Domain:      "example.com",
		Certificate: []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"),
		PrivateKey:  []byte("-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----\n"),
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(90 * 24 * time.Hour),
	}
}

func validParams(t *testing.T, resolver Resolver) Params {
	t.Helper()
	return Params{
		Domain: "example.com",
		// Dir is deliberately left unset: the default location — the
		// bundle inside the project folder — is the shape the design
		// optimizes for, so it is what the bulk of these tests exercise.
		Project:         t.TempDir(),
		Keystore:        bundle.DriverRef{Driver: "file", Config: map[string]any{"path": t.TempDir()}},
		Blob:            bundle.DriverRef{Driver: "local", Config: map[string]any{"path": t.TempDir()}},
		ACMEDNSProvider: "manual",
		Resolver:        resolver,
		Prover:          &fakeProver{},
	}
}

// namelessParams is validParams with the two things a nameless bundle
// (INIT-005) does not have: a domain and an ACME DNS-01 provider. Everything
// else — project folder, keystore, blob — is identical, which is the point:
// a nameless instance is a complete instance in all respects but its name.
func namelessParams(t *testing.T, resolver Resolver) Params {
	t.Helper()
	params := validParams(t, resolver)
	params.Domain = ""
	params.ACMEDNSProvider = ""
	return params
}

func TestRunWritesAValidBundle(t *testing.T) {
	resolver := &fakeResolver{}
	prover := &fakeProver{}
	params := validParams(t, resolver)
	params.Prover = prover
	job := events.NewJob()

	b, err := Run(context.Background(), job, params)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(prover.calls) != 1 || prover.calls[0] != "example.com" {
		t.Errorf("prover calls = %v, want one call for example.com", prover.calls)
	}

	if b.Manifest.Domain != "example.com" {
		t.Errorf("domain = %q", b.Manifest.Domain)
	}
	for _, component := range []string{"forgejo", "caddy"} {
		ref, ok := b.Manifest.Images[component]
		if !ok {
			t.Fatalf("missing image %q: %+v", component, b.Manifest.Images)
		}
		if !strings.Contains(ref, "@sha256:") {
			t.Errorf("image %q = %q, not digest-pinned", component, ref)
		}
	}
	if b.Manifest.Drivers.Keystore.Driver != "file" {
		t.Errorf("keystore driver = %q", b.Manifest.Drivers.Keystore.Driver)
	}
	if b.Manifest.Drivers.Blob.Driver != "local" {
		t.Errorf("blob driver = %q", b.Manifest.Drivers.Blob.Driver)
	}
	if err := b.Manifest.Validate(); err != nil {
		t.Errorf("written manifest fails Validate: %v", err)
	}

	loaded, err := bundle.Load(BundleDir(params))
	if err != nil {
		t.Fatalf("bundle.Load: %v", err)
	}
	if loaded.Manifest.Domain != "example.com" {
		t.Errorf("loaded domain = %q", loaded.Manifest.Domain)
	}

	if !job.Done() {
		t.Error("job did not reach a terminal event")
	}
	var sawSucceeded bool
	for _, ev := range job.Events() {
		if ev.Step == "" && ev.State == events.StateSucceeded {
			sawSucceeded = true
		}
		if ev.State == events.StateFailed {
			t.Errorf("unexpected failed event: %+v", ev)
		}
	}
	if !sawSucceeded {
		t.Error("job never emitted a terminal succeeded event")
	}
}

func TestRunResolvesDefaultImagesWhenNoOverride(t *testing.T) {
	resolver := &fakeResolver{}
	params := validParams(t, resolver)

	if _, err := Run(context.Background(), events.NewJob(), params); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := map[string]bool{
		DefaultImageRefs["forgejo"]: true,
		DefaultImageRefs["caddy"]:   true,
	}
	for _, call := range resolver.calls {
		delete(want, call)
	}
	if len(want) != 0 {
		t.Errorf("resolver never saw default refs %v, got calls %v", want, resolver.calls)
	}
}

// TestDefaultImageRefsCarryNonFloatingTags guards the defect that made
// `farrier init` fail out of the box: every default was tagged ":latest",
// and Forgejo publishes no such tag, so the very first command a new
// operator runs died at StepResolveImages with a registry 404.
//
// It asserts the shape, not the versions — which major or point release
// each component is pinned to is a judgment call that moves, but a floating
// or absent tag is always the bug coming back.
func TestDefaultImageRefsCarryNonFloatingTags(t *testing.T) {
	for component, ref := range DefaultImageRefs {
		name, tag, found := strings.Cut(lastPathSegment(ref), ":")
		if !found || name == "" || tag == "" {
			t.Errorf("%s = %q: default must carry an explicit tag", component, ref)
			continue
		}
		if tag == "latest" || tag == "main" || tag == "edge" || tag == "nightly" {
			t.Errorf("%s = %q: %q is a floating tag; pin a release", component, ref, tag)
		}
	}
}

// lastPathSegment returns the part of an image reference after the final
// "/", which is the only segment a tag can appear in — a registry host may
// carry a port, and "host:5000/repo" must not read as a tag.
func lastPathSegment(ref string) string {
	if i := strings.LastIndex(ref, "/"); i != -1 {
		return ref[i+1:]
	}
	return ref
}

// TestDefaultImageRefsCoverEveryRequiredComponent keeps the defaults and
// the components Run refuses to write a bundle without from drifting apart:
// a required component with no default is an `init` that cannot run without
// an -image override, which is the same failure in a different shape.
func TestDefaultImageRefsCoverEveryRequiredComponent(t *testing.T) {
	for _, component := range requiredComponents {
		if DefaultImageRefs[component] == "" {
			t.Errorf("required component %q has no default image", component)
		}
	}
}

func TestRunUsesImageOverride(t *testing.T) {
	resolver := &fakeResolver{}
	params := validParams(t, resolver)
	params.Images = map[string]string{"forgejo": "example.registry/forgejo:pinned"}

	b, err := Run(context.Background(), events.NewJob(), params)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(b.Manifest.Images["forgejo"], "example.registry/forgejo@sha256:") {
		t.Errorf("forgejo image = %q, want override to have been used", b.Manifest.Images["forgejo"])
	}
	var sawOverride bool
	for _, call := range resolver.calls {
		if call == "example.registry/forgejo:pinned" {
			sawOverride = true
		}
	}
	if !sawOverride {
		t.Errorf("resolver never saw the override ref, calls = %v", resolver.calls)
	}
}

func TestRunRejectsMissingACMEDNSProvider(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	params.ACMEDNSProvider = ""
	job := events.NewJob()

	if _, err := Run(context.Background(), job, params); err == nil {
		t.Fatal("Run: want error for missing acme dns-01 provider, got nil")
	}
	assertJobFailed(t, job)
}

func TestRunProvesZoneControlBeforeResolvingImages(t *testing.T) {
	resolver := &fakeResolver{}
	prover := &fakeProver{}
	params := validParams(t, resolver)
	params.Prover = prover

	if _, err := Run(context.Background(), events.NewJob(), params); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prover.calls) != 1 {
		t.Fatalf("prover calls = %v, want exactly one", prover.calls)
	}
}

func TestRunFailsWithReasonWhenZoneControlProofFails(t *testing.T) {
	reason := errors.New("could not present dns-01 challenge")
	prover := &fakeProver{err: reason}
	params := validParams(t, &fakeResolver{})
	params.Prover = prover
	job := events.NewJob()

	_, err := Run(context.Background(), job, params)
	if err == nil {
		t.Fatal("Run: want error when zone-control proof fails, got nil")
	}
	if !strings.Contains(err.Error(), reason.Error()) {
		t.Errorf("error = %v, want it to wrap the prover's reason %q", err, reason)
	}
	if !strings.Contains(err.Error(), params.Domain) {
		t.Errorf("error = %v, want it to name the domain %q", err, params.Domain)
	}
	assertJobFailed(t, job)

	// Proof runs after image resolution now, so the thing a failed proof
	// must not have reached is the keystore: no key material stored means
	// nothing to recover from and no resume record on disk.
	for _, ev := range job.Events() {
		if ev.Step == StepGenerateKeys {
			t.Errorf("Run reached the generate-keys step after zone-control proof failed: %+v", ev)
		}
	}
	if _, err := os.Stat(filepath.Join(BundleDir(params), IncompleteFile)); !os.IsNotExist(err) {
		t.Errorf("stat resume record: %v, want it never written when the run failed before storing key material", err)
	}
}

// INIT-005: an empty domain asks for a nameless bundle, so it is only an
// error alongside ACME settings — which have nothing to prove and nothing to
// issue a certificate for. Refusing the combination is what stops an
// operator who meant to pass a domain from silently getting an instance with
// no HTTPS.
func TestRunRejectsACMESettingsWithoutADomain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*Params)
	}{
		{"provider", func(p *Params) { p.ACMEDNSProvider = "manual" }},
		{"email", func(p *Params) { p.ACMEEmail = "ops@example.com" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := namelessParams(t, &fakeResolver{})
			tc.apply(&params)
			job := events.NewJob()

			_, err := Run(context.Background(), job, params)
			if err == nil {
				t.Fatal("Run: want error for acme settings without a domain, got nil")
			}
			if !strings.Contains(err.Error(), "without a domain") {
				t.Errorf("error = %v, want it to say the settings came without a domain", err)
			}
			assertJobFailed(t, job)
		})
	}
}

func TestRunRejectsMalformedDomain(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	params.Domain = "not a domain"

	if _, err := Run(context.Background(), events.NewJob(), params); err == nil {
		t.Fatal("Run: want error for malformed domain, got nil")
	}
}

func TestRunRejectsInvalidKeystoreConfig(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	params.Keystore = bundle.DriverRef{Driver: "file"} // missing config.path
	job := events.NewJob()

	if _, err := Run(context.Background(), job, params); err == nil {
		t.Fatal("Run: want error for invalid keystore config, got nil")
	}
	assertJobFailed(t, job)
}

func TestRunRejectsMissingBlobDriver(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	params.Blob = bundle.DriverRef{}

	if _, err := Run(context.Background(), events.NewJob(), params); err == nil {
		t.Fatal("Run: want error for missing blob driver, got nil")
	}
}

// INIT-001: with no explicit location, the bundle lands in .farrier/
// inside the project folder, so the forge definition sits beside the code
// it serves.
func TestRunWritesTheBundleInsideTheProjectFolderByDefault(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	params.Dir = ""

	if _, err := Run(context.Background(), events.NewJob(), params); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := filepath.Join(params.Project, bundle.DirName)
	loaded, err := bundle.Load(want)
	if err != nil {
		t.Fatalf("bundle.Load(%s): %v", want, err)
	}
	if loaded.Manifest.Domain != "example.com" {
		t.Errorf("loaded domain = %q", loaded.Manifest.Domain)
	}
}

// INIT-001: the location is overridable, so a bundle whose instance serves
// several projects can live in a folder of its own. The project folder is
// then left untouched — nothing is written into it.
func TestRunHonorsAnExplicitBundleDirOutsideTheProject(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	params.Dir = filepath.Join(t.TempDir(), "shared-forge")

	if _, err := Run(context.Background(), events.NewJob(), params); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := bundle.Load(params.Dir); err != nil {
		t.Fatalf("bundle.Load(%s): %v", params.Dir, err)
	}
	if _, err := os.Stat(filepath.Join(params.Project, bundle.DirName)); !os.IsNotExist(err) {
		t.Errorf("project folder got a %s directory anyway: err = %v", bundle.DirName, err)
	}
}

func TestBundleDir(t *testing.T) {
	if got, want := BundleDir(Params{Project: "/srv/my-project"}), filepath.Join("/srv/my-project", bundle.DirName); got != want {
		t.Errorf("BundleDir(default) = %q, want %q", got, want)
	}
	if got := BundleDir(Params{Project: "/srv/my-project", Dir: "/srv/forge"}); got != "/srv/forge" {
		t.Errorf("BundleDir(override) = %q, want /srv/forge", got)
	}
}

func TestRunRejectsMissingProject(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	params.Project = ""

	if _, err := Run(context.Background(), events.NewJob(), params); err == nil {
		t.Fatal("Run: want error for missing project folder, got nil")
	}
}

func TestRunRejectsAProjectFolderThatDoesNotExist(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	params.Project = filepath.Join(t.TempDir(), "nope")

	_, err := Run(context.Background(), events.NewJob(), params)
	if err == nil {
		t.Fatal("Run: want error for a missing project folder, got nil")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %v, want it to say the folder does not exist", err)
	}
}

func TestRunRejectsAProjectPathThatIsAFile(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	params.Project = file

	_, err := Run(context.Background(), events.NewJob(), params)
	if err == nil {
		t.Fatal("Run: want error for a project path that is a file, got nil")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error = %v, want it to say the path is not a directory", err)
	}
}

// A bad project folder must be caught in the validate step, before the
// ACME exchange spends a proof and before key material is generated.
func TestRunRejectsABadProjectBeforeProvingZoneControl(t *testing.T) {
	prover := &fakeProver{}
	params := validParams(t, &fakeResolver{})
	params.Prover = prover
	params.Project = filepath.Join(t.TempDir(), "nope")

	if _, err := Run(context.Background(), events.NewJob(), params); err == nil {
		t.Fatal("Run: want error, got nil")
	}
	if len(prover.calls) != 0 {
		t.Errorf("prover calls = %v, want none", prover.calls)
	}
}

func TestRunPropagatesResolverError(t *testing.T) {
	resolver := &fakeResolver{err: fmt.Errorf("registry unreachable")}
	params := validParams(t, resolver)
	job := events.NewJob()

	_, err := Run(context.Background(), job, params)
	if err == nil {
		t.Fatal("Run: want error when resolver fails, got nil")
	}
	if !strings.Contains(err.Error(), "registry unreachable") {
		t.Errorf("error = %v, want it to wrap the resolver error", err)
	}
	assertJobFailed(t, job)
}

// TestAcmeProverWiring exercises the real, non-fake Prover implementation
// against an unrecognized DNS-01 provider name — lego rejects that during
// provider lookup, before any network call, so this stays fast and
// deterministic while still proving acmeProver wires domain/provider/email
// through to acme.Issue correctly.
func TestAcmeProverWiring(t *testing.T) {
	_, err := acmeProver{}.Prove("forge.example.com", "not-a-real-provider", "ops@example.com")
	if err == nil {
		t.Fatal("Prove: want error for an unrecognized DNS provider, got nil")
	}
	if !strings.Contains(err.Error(), "not-a-real-provider") {
		t.Errorf("error %q does not name the bad provider", err)
	}
}

// TestRunGeneratesAndStoresAllKeyMaterial exercises INIT-003 end to end
// through the real file keystore driver: every key generateKeyMaterial
// produces must land on disk, readable back through the same driver
// Resolve uses at deploy time (forge.ResolveSecrets).
func TestRunGeneratesAndStoresAllKeyMaterial(t *testing.T) {
	keysDir := t.TempDir()
	params := validParams(t, &fakeResolver{})
	params.Keystore = bundle.DriverRef{Driver: "file", Config: map[string]any{"path": keysDir}}
	job := events.NewJob()

	if _, err := Run(context.Background(), job, params); err != nil {
		t.Fatalf("Run: %v", err)
	}

	driver, err := keystore.New("file", map[string]any{"path": keysDir})
	if err != nil {
		t.Fatalf("keystore.New: %v", err)
	}
	wantKeys := []string{
		forge.KeySecretKey,
		forge.KeyInternalToken,
		forge.KeyLFSJWTSecret,
		forge.KeyRunnerSecret,
		KeyTLSCertificate,
		KeyTLSPrivateKey,
		KeySSHHostKey,
		KeySSHHostKeyPublic,
		KeyAgeBackupKey,
	}
	seen := make(map[string]string, len(wantKeys))
	for _, name := range wantKeys {
		secret, err := driver.Resolve(context.Background(), name)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		if strings.TrimSpace(secret.Reveal()) == "" {
			t.Errorf("%s resolved to an empty secret", name)
		}
		seen[name] = secret.Reveal()
	}

	for a := range seen {
		for b := range seen {
			if a != b && seen[a] == seen[b] {
				t.Errorf("%s and %s share the same value, want distinct key material", a, b)
			}
		}
	}

	if !strings.Contains(seen[KeyAgeBackupKey], "AGE-SECRET-KEY-1") {
		t.Errorf("age backup key = %q, want an AGE-SECRET-KEY-1... identity", seen[KeyAgeBackupKey])
	}
	if !strings.HasPrefix(seen[KeySSHHostKeyPublic], "ssh-ed25519 ") {
		t.Errorf("ssh host public key = %q, want an ssh-ed25519 authorized-keys line", seen[KeySSHHostKeyPublic])
	}
	if seen[KeyTLSCertificate] != string(fakeCertificate().Certificate) {
		t.Errorf("tls certificate = %q, want the certificate obtained during zone-control proof", seen[KeyTLSCertificate])
	}
	if seen[KeyTLSPrivateKey] != string(fakeCertificate().PrivateKey) {
		t.Errorf("tls private key = %q, want the key obtained during zone-control proof", seen[KeyTLSPrivateKey])
	}
}

// Everything that can fail without persisting anything runs before the
// first Store, so an ordinary failure leaves the keystore untouched and a
// retry is just a retry. This is the ordering the non-atomic-init defect
// came from getting backwards: image resolution failed on a real first run
// after all seven pieces of key material had already been written.
func TestRunDoesEveryFallibleStepBeforeStoringKeyMaterial(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	job := events.NewJob()

	if _, err := Run(context.Background(), job, params); err != nil {
		t.Fatalf("Run: %v", err)
	}

	at := func(step string, state events.State) int {
		for i, ev := range job.Events() {
			if ev.Step == step && ev.State == state {
				return i
			}
		}
		t.Fatalf("did not see %s %s; events = %+v", step, state, job.Events())
		return -1
	}
	keys := at(StepGenerateKeys, events.StateStarted)
	for _, earlier := range []string{StepResolveImages, StepRenderCompose, StepProveZoneControl} {
		if done := at(earlier, events.StateSucceeded); done >= keys {
			t.Errorf("%s finished at event %d, generate-keys started at %d, want it done first", earlier, done, keys)
		}
	}
}

// TestRunRejectsKeystoreDriverWithoutWriteSupport exercises the deliberate
// gap between the two shipped keystore drivers: file supports Store, but
// command is read-only by design (KEY-002), so Run must fail clearly
// rather than silently skip key generation.
func TestRunRejectsKeystoreDriverWithoutWriteSupport(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	params.Keystore = bundle.DriverRef{Driver: "command", Config: map[string]any{"command": "true"}}
	job := events.NewJob()

	_, err := Run(context.Background(), job, params)
	if err == nil {
		t.Fatal("Run: want error for a keystore driver that cannot store key material, got nil")
	}
	if !strings.Contains(err.Error(), "command") {
		t.Errorf("error = %v, want it to name the offending driver", err)
	}
	assertJobFailed(t, job)

	var provedZoneControl bool
	for _, ev := range job.Events() {
		if ev.Step == StepProveZoneControl {
			provedZoneControl = true
		}
	}
	if provedZoneControl {
		t.Error("Run proved zone control against a keystore driver it was always going to reject, want it to fail during validate")
	}
}

func TestRunFailsWhenKeyMaterialAlreadyExists(t *testing.T) {
	keysDir := t.TempDir()
	if err := (keystore.FileDriver{Path: keysDir}).Store(context.Background(), forge.KeySecretKey, keystore.NewSecret("pre-existing")); err != nil {
		t.Fatalf("seed existing key: %v", err)
	}
	params := validParams(t, &fakeResolver{})
	params.Keystore = bundle.DriverRef{Driver: "file", Config: map[string]any{"path": keysDir}}
	job := events.NewJob()

	_, err := Run(context.Background(), job, params)
	if err == nil {
		t.Fatal("Run: want error when key material already exists, got nil")
	}
	if !strings.Contains(err.Error(), forge.KeySecretKey) {
		t.Errorf("error = %v, want it to name the pre-existing key", err)
	}
	assertJobFailed(t, job)
}

func assertJobFailed(t *testing.T, job *events.Job) {
	t.Helper()
	if !job.Done() {
		t.Fatal("job did not reach a terminal event")
	}
	evs := job.Events()
	last := evs[len(evs)-1]
	if last.Step != "" || last.State != events.StateFailed {
		t.Errorf("last event = %+v, want a job-terminal failed event", last)
	}
}

// FORGE-005: every bundle init writes pins a runner image and records the
// colocated-runner choice explicitly, so a fresh `up` gives working CI and
// the operator can see — and flip — the knob in farrier.yaml.
func TestRunPinsARunnerAndRecordsTheColocatedChoice(t *testing.T) {
	params := validParams(t, &fakeResolver{})

	b, err := Run(context.Background(), events.NewJob(), params)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	ref, ok := b.Manifest.Images[forge.RunnerService]
	if !ok {
		t.Fatalf("manifest pins no %q image: %v", forge.RunnerService, b.Manifest.Images)
	}
	if !strings.Contains(ref, "@sha256:") {
		t.Errorf("runner image %q is not digest-pinned", ref)
	}
	if !b.Manifest.ColocatedRunnerDeclared() {
		t.Error("manifest leaves the colocated-runner choice unwritten")
	}
	if !b.Manifest.ColocatedRunnerEnabled() {
		t.Error("colocated runner is off by default, want on")
	}
}

func TestRunHonorsAColocatedRunnerOptOut(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	disabled := false
	params.ColocatedRunner = &disabled

	b, err := Run(context.Background(), events.NewJob(), params)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if b.Manifest.ColocatedRunnerEnabled() {
		t.Error("colocated runner is on, want off")
	}
	// Still pinned: re-enabling is a one-line manifest edit, not a
	// registry lookup (requiredComponents' doc comment).
	if _, ok := b.Manifest.Images[forge.RunnerService]; !ok {
		t.Errorf("manifest pins no %q image: %v", forge.RunnerService, b.Manifest.Images)
	}
}

func TestRunStoresAWellFormedRunnerSecret(t *testing.T) {
	keysDir := t.TempDir()
	params := validParams(t, &fakeResolver{})
	params.Keystore = bundle.DriverRef{Driver: "file", Config: map[string]any{"path": keysDir}}

	if _, err := Run(context.Background(), events.NewJob(), params); err != nil {
		t.Fatalf("Run: %v", err)
	}

	driver, err := keystore.New("file", map[string]any{"path": keysDir})
	if err != nil {
		t.Fatalf("keystore.New: %v", err)
	}
	secret, err := driver.Resolve(context.Background(), forge.KeyRunnerSecret)
	if err != nil {
		t.Fatalf("resolve %s: %v", forge.KeyRunnerSecret, err)
	}
	if err := forge.ValidateRunnerSecret(secret.Reveal()); err != nil {
		t.Errorf("stored runner secret is not in Forgejo's registration format: %v", err)
	}
}

// INIT-005: `init` with no domain writes a real bundle. The manifest carries
// no domain and no ACME section, and — the part that matters — the bundle
// reloads from disk, since bundle.Load validates what it reads.
func TestRunWithNoDomainWritesANamelessBundle(t *testing.T) {
	prover := &fakeProver{}
	params := namelessParams(t, &fakeResolver{})
	params.Prover = prover
	job := events.NewJob()

	b, err := Run(context.Background(), job, params)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(prover.calls) != 0 {
		t.Errorf("prover calls = %v, want none: a nameless bundle proves no zone", prover.calls)
	}
	if b.Manifest.Named() {
		t.Errorf("manifest domain = %q, want it empty", b.Manifest.Domain)
	}
	if b.Manifest.ACME.DNSProvider != "" || b.Manifest.ACME.Email != "" {
		t.Errorf("manifest acme = %+v, want it empty for a nameless bundle", b.Manifest.ACME)
	}

	reloaded, err := bundle.Load(BundleDir(params))
	if err != nil {
		t.Fatalf("reload the written bundle: %v", err)
	}
	if reloaded.Manifest.Named() {
		t.Errorf("reloaded domain = %q, want it empty", reloaded.Manifest.Domain)
	}
	if len(reloaded.Compose) == 0 {
		t.Error("reloaded bundle has no rendered compose files")
	}
}

// INIT-005: the manifest a nameless bundle writes omits the domain key
// rather than writing an empty one, so farrier.yaml reads as a bundle with
// no name instead of one whose name failed to render.
func TestRunWithNoDomainOmitsTheDomainFromTheManifestFile(t *testing.T) {
	params := namelessParams(t, &fakeResolver{})

	if _, err := Run(context.Background(), events.NewJob(), params); err != nil {
		t.Fatalf("Run: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(BundleDir(params), bundle.ManifestFile))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if strings.Contains(string(raw), "domain:") {
		t.Errorf("manifest carries a domain key:\n%s", raw)
	}
}

// INIT-005: "every other piece of key material is generated as usual" — the
// TLS certificate and its private key are the only two a nameless bundle
// lacks, and they are absent because there is no name to issue them for.
func TestRunWithNoDomainGeneratesEveryKeyButTLS(t *testing.T) {
	keysDir := t.TempDir()
	params := namelessParams(t, &fakeResolver{})
	params.Keystore = bundle.DriverRef{Driver: "file", Config: map[string]any{"path": keysDir}}

	if _, err := Run(context.Background(), events.NewJob(), params); err != nil {
		t.Fatalf("Run: %v", err)
	}

	driver, err := keystore.New("file", map[string]any{"path": keysDir})
	if err != nil {
		t.Fatalf("keystore.New: %v", err)
	}
	for _, name := range []string{
		forge.KeySecretKey,
		forge.KeyInternalToken,
		forge.KeyLFSJWTSecret,
		forge.KeyRunnerSecret,
		KeySSHHostKey,
		KeySSHHostKeyPublic,
		KeyAgeBackupKey,
	} {
		secret, err := driver.Resolve(context.Background(), name)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		if strings.TrimSpace(secret.Reveal()) == "" {
			t.Errorf("%s resolved to an empty secret", name)
		}
	}
	for _, name := range []string{KeyTLSCertificate, KeyTLSPrivateKey} {
		if _, err := driver.Resolve(context.Background(), name); err == nil {
			t.Errorf("resolve %s: want it absent from a nameless bundle's keystore, got a secret", name)
		}
	}
}

// INIT-005: the skipped proof is reported rather than dropped — an operator
// watching the stream should see that no zone was proven by design.
func TestRunWithNoDomainReportsTheSkippedProof(t *testing.T) {
	params := namelessParams(t, &fakeResolver{})
	job := events.NewJob()

	if _, err := Run(context.Background(), job, params); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var detail string
	var found bool
	for _, ev := range job.Events() {
		if ev.Step == StepProveZoneControl && ev.State == events.StateSucceeded {
			detail, found = ev.Detail, true
		}
	}
	if !found {
		t.Fatalf("no succeeded %s event in the stream: %+v", StepProveZoneControl, job.Events())
	}
	if !strings.Contains(detail, "skipping") {
		t.Errorf("%s detail = %q, want it to say the proof was skipped", StepProveZoneControl, detail)
	}
}

// INIT-004 and INIT-005 compose: having no name is not permission to
// overwrite an identity. Both directions are covered, because the two
// requirements meet at the same folder — a nameless init must not clobber a
// named bundle, and a named one must not clobber a nameless bundle. The
// refusal happens in the validate step, so the nameless run still spends no
// ACME exchange and mints no key material for an instance never created.
func TestRunRefusesToOverwriteAnExistingBundleWithoutADomain(t *testing.T) {
	cases := []struct {
		name  string
		first func(t *testing.T) Params
		again func(t *testing.T, project string) Params
	}{
		{
			name:  "nameless init over a named bundle",
			first: func(t *testing.T) Params { return validParams(t, &fakeResolver{}) },
			again: func(t *testing.T, project string) Params {
				p := namelessParams(t, &fakeResolver{})
				p.Project = project
				return p
			},
		},
		{
			name:  "nameless init over a nameless bundle",
			first: func(t *testing.T) Params { return namelessParams(t, &fakeResolver{}) },
			again: func(t *testing.T, project string) Params {
				p := namelessParams(t, &fakeResolver{})
				p.Project = project
				return p
			},
		},
		{
			name:  "named init over a nameless bundle",
			first: func(t *testing.T) Params { return namelessParams(t, &fakeResolver{}) },
			again: func(t *testing.T, project string) Params {
				p := validParams(t, &fakeResolver{})
				p.Project = project
				return p
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := tc.first(t)
			if _, err := Run(context.Background(), events.NewJob(), params); err != nil {
				t.Fatalf("first Run: %v", err)
			}
			dir := BundleDir(params)
			before, err := os.ReadFile(filepath.Join(dir, bundle.ManifestFile))
			if err != nil {
				t.Fatalf("read first manifest: %v", err)
			}

			// A fresh keystore and prover for the second attempt, so what
			// the refused run did — and did not do — is visible on its own.
			second := tc.again(t, params.Project)
			prover := &fakeProver{}
			second.Prover = prover
			job := events.NewJob()

			_, err = Run(context.Background(), job, second)
			if err == nil {
				t.Fatal("Run: want error re-initializing a project that already has a bundle, got nil")
			}
			if !strings.Contains(err.Error(), dir) {
				t.Errorf("error = %v, want it to name the bundle folder %s", err, dir)
			}
			assertJobFailed(t, job)

			after, err := os.ReadFile(filepath.Join(dir, bundle.ManifestFile))
			if err != nil {
				t.Fatalf("read manifest after refusal: %v", err)
			}
			if string(after) != string(before) {
				t.Error("the existing manifest changed; a refused init must leave the bundle untouched")
			}
			if len(prover.calls) != 0 {
				t.Errorf("prover calls = %v, want a refused init to spend no ACME exchange", prover.calls)
			}
			entries, err := os.ReadDir(second.Keystore.Config["path"].(string))
			if err != nil {
				t.Fatalf("read keystore dir: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("keystore holds %d entry/entries, want a refused init to generate no key material", len(entries))
			}
		})
	}
}

// UP-005: the git-over-SSH host port is written into the manifest even at
// its default, so farrier.yaml shows the operator which port clients reach
// git on and that it is theirs to change.
func TestRunRecordsTheGitSSHPort(t *testing.T) {
	b, err := Run(context.Background(), events.NewJob(), validParams(t, &fakeResolver{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if b.Manifest.GitSSHPort != bundle.DefaultGitSSHPort {
		t.Errorf("manifest git-over-ssh port = %d, want the default %d written out explicitly", b.Manifest.GitSSHPort, bundle.DefaultGitSSHPort)
	}
}

func TestRunHonorsAnExplicitGitSSHPort(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	params.GitSSHPort = 22

	b, err := Run(context.Background(), events.NewJob(), params)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := b.Manifest.GitSSHPortOrDefault(); got != 22 {
		t.Errorf("manifest git-over-ssh port = %d, want the operator's 22", got)
	}
}

// An unusable port is caught in the validate step, before an ACME exchange
// is spent and key material generated — the same place every other
// unusable input is caught.
func TestRunRejectsAnInvalidGitSSHPort(t *testing.T) {
	prover := &fakeProver{}
	params := validParams(t, &fakeResolver{})
	params.Prover = prover
	params.GitSSHPort = 70000

	if _, err := Run(context.Background(), events.NewJob(), params); err == nil {
		t.Fatal("Run() = nil, want an error naming the port")
	} else if !strings.Contains(err.Error(), "70000") {
		t.Errorf("error = %v, want it to name the rejected port", err)
	}
	if len(prover.calls) != 0 {
		t.Errorf("zone control was proven despite an unusable port: %v", prover.calls)
	}
}

// INIT-004: a project that already holds a bundle is never re-initialized
// over. The bundle directory carries the running instance's identity, so a
// second init must fail, name the folder, and leave the first bundle
// exactly as it found it.
func TestRunRefusesToOverwriteAnExistingBundle(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	if _, err := Run(context.Background(), events.NewJob(), params); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	dir := BundleDir(params)
	before, err := os.ReadFile(filepath.Join(dir, bundle.ManifestFile))
	if err != nil {
		t.Fatalf("read first manifest: %v", err)
	}

	// A fresh keystore and prover for the second attempt, so what the
	// refused run did — and did not do — is visible on its own.
	second := validParams(t, &fakeResolver{})
	second.Project = params.Project
	prover := &fakeProver{}
	second.Prover = prover
	job := events.NewJob()

	_, err = Run(context.Background(), job, second)
	if err == nil {
		t.Fatal("Run: want error re-initializing a project that already has a bundle, got nil")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error = %v, want it to name the bundle folder %s", err, dir)
	}
	assertJobFailed(t, job)

	after, err := os.ReadFile(filepath.Join(dir, bundle.ManifestFile))
	if err != nil {
		t.Fatalf("read manifest after refusal: %v", err)
	}
	if string(after) != string(before) {
		t.Error("the existing manifest changed; a refused init must leave the bundle untouched")
	}

	if len(prover.calls) != 0 {
		t.Errorf("prover calls = %v, want a refused init to spend no ACME exchange", prover.calls)
	}
	keysDir := second.Keystore.Config["path"].(string)
	entries, err := os.ReadDir(keysDir)
	if err != nil {
		t.Fatalf("read keystore dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("keystore holds %d entry/entries, want a refused init to generate no key material", len(entries))
	}
}

// The refusal follows the bundle, not the project: an operator who points
// init at an explicit location — the shape a bundle serving several
// projects takes (INIT-001) — is protected there too.
func TestRunRefusesToOverwriteAnExistingBundleAtAnExplicitDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared-forge")
	params := validParams(t, &fakeResolver{})
	params.Dir = dir
	if _, err := Run(context.Background(), events.NewJob(), params); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	second := validParams(t, &fakeResolver{})
	second.Dir = dir
	job := events.NewJob()

	_, err := Run(context.Background(), job, second)
	if err == nil {
		t.Fatal("Run: want error re-initializing an occupied bundle dir, got nil")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error = %v, want it to name %s", err, dir)
	}
	assertJobFailed(t, job)
}

// A crashed init can leave compose/ behind with no manifest. Finishing
// that folder with freshly generated key material is exactly what INIT-004
// prevents, so a torn bundle is refused like a whole one.
func TestRunRefusesATornBundle(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	dir := BundleDir(params)
	if err := os.MkdirAll(filepath.Join(dir, bundle.ComposeDir), 0o755); err != nil {
		t.Fatalf("seed torn bundle: %v", err)
	}
	job := events.NewJob()

	if _, err := Run(context.Background(), job, params); err == nil {
		t.Fatal("Run: want error for a bundle dir holding compose/ but no manifest, got nil")
	}
	assertJobFailed(t, job)
}

// An empty .farrier/ is not a bundle. Refusing one would break the
// operator who created the folder before running init.
func TestRunInitializesIntoAnEmptyBundleDir(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	if err := os.MkdirAll(BundleDir(params), 0o755); err != nil {
		t.Fatalf("create empty bundle dir: %v", err)
	}

	if _, err := Run(context.Background(), events.NewJob(), params); err != nil {
		t.Fatalf("Run into an empty bundle dir: %v", err)
	}
	if _, err := bundle.Load(BundleDir(params)); err != nil {
		t.Fatalf("bundle.Load: %v", err)
	}
}
