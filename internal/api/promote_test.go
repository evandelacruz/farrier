package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/dns"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/promote"
)

// writeTestSnapshot puts a snapshot object at dir under key, so a test
// exercising handlePromote past FAIL-002's confirmation gate has a
// resolvable snapshot to promote.
func writeTestSnapshot(t *testing.T, dir, key string) {
	t.Helper()
	adapter, err := backup.OpenDestination(dir)
	if err != nil {
		t.Fatalf("OpenDestination: %v", err)
	}
	if err := adapter.Put(context.Background(), key, bytes.NewReader([]byte("snapshot")), 8); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

// fakePromoteHost implements promote.Host (restore.Host: deploy.Host +
// RunStdin) without a real SSH connection — the same shape
// fakeRestoreHost (restore_test.go) takes.
type fakePromoteHost struct {
	closedCh chan struct{}
}

func newFakePromoteHost() *fakePromoteHost {
	return &fakePromoteHost{closedCh: make(chan struct{})}
}

func (f *fakePromoteHost) Output(ctx context.Context, command string) ([]byte, error) {
	return nil, nil
}
func (f *fakePromoteHost) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	return nil
}
func (f *fakePromoteHost) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	return nil
}
func (f *fakePromoteHost) CheckHost(ctx context.Context) error { return nil }
func (f *fakePromoteHost) Close() error                        { close(f.closedCh); return nil }
func (f *fakePromoteHost) RunStdin(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	io.Copy(io.Discard, stdin)
	return nil
}

func (f *fakePromoteHost) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-f.closedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("host was not closed in time")
	}
}

func doPromote(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/promote", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandlePromoteBadJSON(t *testing.T) {
	s := newTestServer()
	rec := doPromote(t, s, "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandlePromoteMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing bundleDir", `{"target":"ssh://user@host","from":"/tmp/src"}`},
		{"missing target", `{"bundleDir":"/tmp/bundle","from":"/tmp/src"}`},
		{"missing from", `{"bundleDir":"/tmp/bundle","target":"ssh://user@host"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer()
			rec := doPromote(t, s, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestHandlePromoteRefusesWithoutConfirm(t *testing.T) {
	s := newTestServer()
	from := t.TempDir()
	writeTestSnapshot(t, from, "20260101T000000Z.age")
	s.promoteRun = func(ctx context.Context, job *events.Job, opts promote.Options) error {
		t.Fatal("promoteRun should not be called when confirm is not set")
		return nil
	}

	rec := doPromote(t, s, fmt.Sprintf(`{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":%q}`, from))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "20260101T000000Z.age") {
		t.Errorf("body %q does not name the resolved snapshot", rec.Body.String())
	}
}

func TestHandlePromoteRefusesWithConfirmFalse(t *testing.T) {
	s := newTestServer()
	from := t.TempDir()
	writeTestSnapshot(t, from, "20260101T000000Z.age")
	s.promoteRun = func(ctx context.Context, job *events.Job, opts promote.Options) error {
		t.Fatal("promoteRun should not be called when confirm is false")
		return nil
	}

	rec := doPromote(t, s, fmt.Sprintf(`{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":%q,"confirm":false}`, from))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestHandlePromoteConfirmTrueStartsJobWithNoSnapshots(t *testing.T) {
	s := newTestServer()
	from := t.TempDir() // no snapshots written
	rec := doPromote(t, s, fmt.Sprintf(`{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":%q,"confirm":true}`, from))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no snapshots") {
		t.Errorf("body %q does not explain the destination has no snapshots", rec.Body.String())
	}
}

func TestHandlePromoteInvalidTimeout(t *testing.T) {
	s := newTestServer()
	from := t.TempDir()
	writeTestSnapshot(t, from, "20260101T000000Z.age")
	rec := doPromote(t, s, fmt.Sprintf(`{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":%q,"confirm":true,"sshTimeout":"not-a-duration"}`, from))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandlePromoteInvalidTarget(t *testing.T) {
	s := newTestServer()
	from := t.TempDir()
	writeTestSnapshot(t, from, "20260101T000000Z.age")
	// No explicit dnsValue: handlePromote must parse target to default it,
	// and a target that doesn't parse must fail the request outright
	// rather than reaching dialPromote with a garbage dnsValue.
	rec := doPromote(t, s, fmt.Sprintf(`{"bundleDir":"/tmp/bundle","target":"not-a-target","from":%q,"confirm":true}`, from))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandlePromoteBundleLoadError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) {
		return nil, errors.New("no such bundle")
	}
	from := t.TempDir()
	writeTestSnapshot(t, from, "20260101T000000Z.age")
	rec := doPromote(t, s, fmt.Sprintf(`{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":%q,"confirm":true}`, from))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandlePromoteDialFailureFailsJob(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dialPromote = func(ctx context.Context, target string, opts orchestrate.Options) (promote.Host, error) {
		return nil, errors.New("connection refused")
	}
	s.promoteRun = func(ctx context.Context, job *events.Job, opts promote.Options) error {
		t.Fatal("promoteRun should not be called when dial fails")
		return nil
	}

	from := t.TempDir()
	writeTestSnapshot(t, from, "20260101T000000Z.age")
	rec := doPromote(t, s, fmt.Sprintf(`{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":%q,"confirm":true}`, from))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	assertLastEventFailed(t, job)
}

func TestHandlePromoteKeystoreBuildError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	host := newFakePromoteHost()
	s.dialPromote = func(ctx context.Context, target string, opts orchestrate.Options) (promote.Host, error) {
		return host, nil
	}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return nil, errors.New("bad driver config")
	}

	from := t.TempDir()
	writeTestSnapshot(t, from, "20260101T000000Z.age")
	rec := doPromote(t, s, fmt.Sprintf(`{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":%q,"confirm":true}`, from))
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	assertLastEventFailed(t, job)
	host.waitClosed(t)
}

func TestHandlePromoteDNSResolveError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	host := newFakePromoteHost()
	s.dialPromote = func(ctx context.Context, target string, opts orchestrate.Options) (promote.Host, error) {
		return host, nil
	}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return newFakeAgeKeystore(t), nil
	}
	s.newBlob = func(driverName string, config map[string]any) (blob.Adapter, error) {
		return blob.NewLocal(t.TempDir())
	}
	s.resolveDNS = func(ctx context.Context, job *events.Job, ref bundle.DriverRef, keystoreDriver keystore.Driver) (dns.Driver, error) {
		return nil, errors.New("bad dns config")
	}

	from := t.TempDir()
	writeTestSnapshot(t, from, "20260101T000000Z.age")
	rec := doPromote(t, s, fmt.Sprintf(`{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":%q,"confirm":true}`, from))
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	assertLastEventFailed(t, job)
	host.waitClosed(t)
}

func TestHandlePromoteSuccessWiresOptions(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) {
		if dir != "/tmp/bundle" {
			t.Errorf("loadBundle dir = %q, want /tmp/bundle", dir)
		}
		return testBundle(), nil
	}
	host := newFakePromoteHost()
	var gotTarget string
	s.dialPromote = func(ctx context.Context, target string, opts orchestrate.Options) (promote.Host, error) {
		gotTarget = target
		return host, nil
	}
	ageKeystore := newFakeAgeKeystore(t)
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		if driverName != "file" {
			t.Errorf("keystore driverName = %q, want file", driverName)
		}
		return ageKeystore, nil
	}
	s.newBlob = func(driverName string, config map[string]any) (blob.Adapter, error) {
		if driverName != "local" {
			t.Errorf("blob driverName = %q, want local", driverName)
		}
		return blob.NewLocal(t.TempDir())
	}
	var gotRef bundle.DriverRef
	fakeDNS := &recordingDNSDriver{}
	s.resolveDNS = func(ctx context.Context, job *events.Job, ref bundle.DriverRef, keystoreDriver keystore.Driver) (dns.Driver, error) {
		gotRef = ref
		return fakeDNS, nil
	}
	var gotOpts promote.Options
	s.promoteRun = func(ctx context.Context, job *events.Job, opts promote.Options) error {
		gotOpts = opts
		job.Succeeded("failed over")
		return nil
	}

	from := t.TempDir()
	writeTestSnapshot(t, from, "20260101T000000Z.age")
	rec := doPromote(t, s, fmt.Sprintf(`{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":%q,"snapshot":"20260101T000000Z.age","remoteDir":"/srv/farrier","dnsValue":"203.0.113.10","confirm":true}`, from))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	evs := job.Events()
	if last := evs[len(evs)-1]; last.State != events.StateSucceeded {
		t.Fatalf("last event = %+v, want succeeded", last)
	}

	if gotTarget != "ssh://user@host" {
		t.Errorf("target = %q, want ssh://user@host", gotTarget)
	}
	if gotOpts.RemoteDir != "/srv/farrier" {
		t.Errorf("RemoteDir = %q, want /srv/farrier", gotOpts.RemoteDir)
	}
	if gotOpts.SnapshotKey != "20260101T000000Z.age" {
		t.Errorf("SnapshotKey = %q, want 20260101T000000Z.age", gotOpts.SnapshotKey)
	}
	if gotOpts.DNSValue != "203.0.113.10" {
		t.Errorf("DNSValue = %q, want 203.0.113.10", gotOpts.DNSValue)
	}
	if gotOpts.WorkDir == "" {
		t.Error("WorkDir is empty, want a generated scratch directory")
	}
	if gotOpts.Identity == nil {
		t.Error("Identity is nil, want the resolved age backup key")
	}
	if gotOpts.Source == nil || gotOpts.Blobs == nil || gotOpts.Keystore == nil || gotOpts.Host == nil || gotOpts.Bundle == nil || gotOpts.DNS == nil {
		t.Errorf("Options is missing a required field: %+v", gotOpts)
	}
	if gotRef.Driver != "" {
		t.Errorf("resolveDNS ref.Driver = %q, want empty (testBundle configures no DNS driver)", gotRef.Driver)
	}
	host.waitClosed(t)
}

func TestHandlePromoteDefaultsDNSValueFromTarget(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dialPromote = func(ctx context.Context, target string, opts orchestrate.Options) (promote.Host, error) {
		return newFakePromoteHost(), nil
	}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return newFakeAgeKeystore(t), nil
	}
	s.newBlob = func(driverName string, config map[string]any) (blob.Adapter, error) {
		return blob.NewLocal(t.TempDir())
	}
	s.resolveDNS = func(ctx context.Context, job *events.Job, ref bundle.DriverRef, keystoreDriver keystore.Driver) (dns.Driver, error) {
		return &recordingDNSDriver{}, nil
	}
	var gotOpts promote.Options
	s.promoteRun = func(ctx context.Context, job *events.Job, opts promote.Options) error {
		gotOpts = opts
		job.Succeeded("done")
		return nil
	}

	from := t.TempDir()
	writeTestSnapshot(t, from, "20260101T000000Z.age")
	rec := doPromote(t, s, fmt.Sprintf(`{"bundleDir":"/tmp/bundle","target":"ssh://user@standby.example.com:2222","from":%q,"confirm":true}`, from))
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)

	if gotOpts.DNSValue != "standby.example.com" {
		t.Errorf("DNSValue = %q, want standby.example.com (parsed from target)", gotOpts.DNSValue)
	}
	if gotOpts.RemoteDir != defaultRemoteDir {
		t.Errorf("RemoteDir = %q, want default %q", gotOpts.RemoteDir, defaultRemoteDir)
	}
}

// recordingDNSDriver is a minimal dns.Driver stub for tests that only need
// promote.Options.DNS to be non-nil.
type recordingDNSDriver struct{}

func (*recordingDNSDriver) Set(ctx context.Context, record, value string, ttl time.Duration) error {
	return nil
}
func (*recordingDNSDriver) Delete(ctx context.Context, record string) error { return nil }
