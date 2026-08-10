package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// fakeHost implements Host (deploy.Host + Close) without a real SSH
// connection, so handleUp's dial-then-deploy sequencing is testable.
// Close happens in a goroutine handleUp spawns, concurrently with the test
// goroutine, so closed is reported through a channel rather than a plain
// bool the test would have to read unsynchronized.
type fakeHost struct {
	closedCh chan struct{}
}

func newFakeHost() *fakeHost {
	return &fakeHost{closedCh: make(chan struct{})}
}

func (f *fakeHost) Output(ctx context.Context, command string) ([]byte, error) { return nil, nil }
func (f *fakeHost) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	return nil
}
func (f *fakeHost) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	return nil
}
func (f *fakeHost) CheckHost(ctx context.Context) error { return nil }
func (f *fakeHost) Close() error                        { close(f.closedCh); return nil }

func (f *fakeHost) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-f.closedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("host was not closed in time")
	}
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
	}
}

func doUp(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/up", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandleUpBadJSON(t *testing.T) {
	s := newTestServer()
	rec := doUp(t, s, "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleUpMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing bundleDir", `{"target":"ssh://user@host"}`},
		{"missing target", `{"bundleDir":"/tmp/bundle"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer()
			rec := doUp(t, s, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestHandleUpInvalidTimeout(t *testing.T) {
	s := newTestServer()
	rec := doUp(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","sshTimeout":"not-a-duration"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleUpBundleLoadError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) {
		return nil, errors.New("no such bundle")
	}
	rec := doUp(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleUpDialFailureFailsJob(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dial = func(ctx context.Context, target string, opts orchestrate.Options) (Host, error) {
		return nil, errors.New("connection refused")
	}
	s.deployUp = func(ctx context.Context, job *events.Job, host deploy.Host, b *bundle.Bundle, opts deploy.Options) error {
		t.Fatal("deployUp should not be called when dial fails")
		return nil
	}

	rec := doUp(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	var resp struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	job, ok := s.jobs.Get(resp.JobID)
	if !ok {
		t.Fatalf("job %q not found", resp.JobID)
	}
	waitDone(t, job)
	evs := job.Events()
	last := evs[len(evs)-1]
	if last.State != events.StateFailed {
		t.Errorf("last event state = %q, want failed", last.State)
	}
}

func TestHandleUpSuccessDialsAndDeploys(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) {
		if dir != "/tmp/bundle" {
			t.Errorf("loadBundle dir = %q, want /tmp/bundle", dir)
		}
		return testBundle(), nil
	}
	host := newFakeHost()
	var gotTarget string
	var gotTimeout time.Duration
	s.dial = func(ctx context.Context, target string, opts orchestrate.Options) (Host, error) {
		gotTarget = target
		gotTimeout = opts.Timeout
		return host, nil
	}
	var gotRemoteDir string
	s.deployUp = func(ctx context.Context, job *events.Job, h deploy.Host, b *bundle.Bundle, opts deploy.Options) error {
		gotRemoteDir = opts.RemoteDir
		job.Succeeded("deployed")
		return nil
	}

	rec := doUp(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","remoteDir":"/srv/farrier","sshTimeout":"5s"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var resp struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	job, ok := s.jobs.Get(resp.JobID)
	if !ok {
		t.Fatalf("job %q not found", resp.JobID)
	}
	waitDone(t, job)

	if gotTarget != "ssh://user@host" {
		t.Errorf("target = %q, want ssh://user@host", gotTarget)
	}
	if gotTimeout != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", gotTimeout)
	}
	if gotRemoteDir != "/srv/farrier" {
		t.Errorf("remoteDir = %q, want /srv/farrier", gotRemoteDir)
	}
	host.waitClosed(t)
}

func TestHandleUpDefaultsRemoteDir(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dial = func(ctx context.Context, target string, opts orchestrate.Options) (Host, error) {
		return newFakeHost(), nil
	}
	var gotRemoteDir string
	s.deployUp = func(ctx context.Context, job *events.Job, h deploy.Host, b *bundle.Bundle, opts deploy.Options) error {
		gotRemoteDir = opts.RemoteDir
		job.Succeeded("deployed")
		return nil
	}

	rec := doUp(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host"}`)
	var resp struct {
		JobID string `json:"jobId"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	job, _ := s.jobs.Get(resp.JobID)
	waitDone(t, job)

	if gotRemoteDir != defaultRemoteDir {
		t.Errorf("remoteDir = %q, want %q", gotRemoteDir, defaultRemoteDir)
	}
}

func waitDone(t *testing.T, job *events.Job) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !job.Done() {
		select {
		case <-deadline:
			t.Fatal("job did not reach terminal state in time")
		case <-time.After(time.Millisecond):
		}
	}
}

// UP-006: a nameless bundle's address is supplied at `up`, and the API is
// the second frontend that has to be able to supply it. The pairing rule
// itself belongs to the core, so all the handler owes is passing it
// through unchanged.
func TestHandleUpPassesTheAddressThrough(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	host := newFakeHost()
	s.dial = func(ctx context.Context, target string, opts orchestrate.Options) (Host, error) {
		return host, nil
	}
	gotAddress := make(chan string, 1)
	s.deployUp = func(ctx context.Context, job *events.Job, h deploy.Host, b *bundle.Bundle, opts deploy.Options) error {
		gotAddress <- opts.Address
		job.Succeeded("deployed")
		return nil
	}

	rec := doUp(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","address":"box.tail1234.ts.net"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	select {
	case got := <-gotAddress:
		if got != "box.tail1234.ts.net" {
			t.Errorf("deploy.Options.Address = %q, want box.tail1234.ts.net", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deployUp was not called in time")
	}
	host.waitClosed(t)
}
