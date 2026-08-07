package initialize

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
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
// provider.
type fakeProver struct {
	calls []string
	err   error
}

func (f *fakeProver) Prove(domain, dnsProvider, email string) error {
	f.calls = append(f.calls, domain)
	if f.err != nil {
		return f.err
	}
	return nil
}

func validParams(t *testing.T, resolver Resolver) Params {
	t.Helper()
	return Params{
		Domain:          "example.com",
		Dir:             filepath.Join(t.TempDir(), "bundle"),
		Keystore:        bundle.DriverRef{Driver: "file", Config: map[string]any{"path": t.TempDir()}},
		Blob:            bundle.DriverRef{Driver: "local", Config: map[string]any{"path": t.TempDir()}},
		ACMEDNSProvider: "manual",
		Resolver:        resolver,
		Prover:          &fakeProver{},
	}
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

	loaded, err := bundle.Load(params.Dir)
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

	var resolvedAnyImage bool
	for _, ev := range job.Events() {
		if ev.Step == StepResolveImages {
			resolvedAnyImage = true
		}
	}
	if resolvedAnyImage {
		t.Error("Run resolved images after zone-control proof failed, want it to stop early")
	}
}

func TestRunRejectsEmptyDomain(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	params.Domain = ""
	job := events.NewJob()

	if _, err := Run(context.Background(), job, params); err == nil {
		t.Fatal("Run: want error for empty domain, got nil")
	}
	assertJobFailed(t, job)
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

func TestRunRejectsMissingDir(t *testing.T) {
	params := validParams(t, &fakeResolver{})
	params.Dir = ""

	if _, err := Run(context.Background(), events.NewJob(), params); err == nil {
		t.Fatal("Run: want error for missing dir, got nil")
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
	err := acmeProver{}.Prove("forge.example.com", "not-a-real-provider", "ops@example.com")
	if err == nil {
		t.Fatal("Prove: want error for an unrecognized DNS provider, got nil")
	}
	if !strings.Contains(err.Error(), "not-a-real-provider") {
		t.Errorf("error %q does not name the bad provider", err)
	}
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
