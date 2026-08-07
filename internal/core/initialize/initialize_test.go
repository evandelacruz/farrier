package initialize

import (
	"context"
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

func validParams(t *testing.T, resolver Resolver) Params {
	t.Helper()
	return Params{
		Domain:   "example.com",
		Dir:      filepath.Join(t.TempDir(), "bundle"),
		Keystore: bundle.DriverRef{Driver: "file", Config: map[string]any{"path": t.TempDir()}},
		Blob:     bundle.DriverRef{Driver: "local", Config: map[string]any{"path": t.TempDir()}},
		Resolver: resolver,
	}
}

func TestRunWritesAValidBundle(t *testing.T) {
	resolver := &fakeResolver{}
	params := validParams(t, resolver)
	job := events.NewJob()

	b, err := Run(context.Background(), job, params)
	if err != nil {
		t.Fatalf("Run: %v", err)
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
