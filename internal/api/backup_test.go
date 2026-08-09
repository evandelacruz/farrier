package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/initialize"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// fakeBackupHost implements backupHost (state.Runner + Target + Close)
// without a real SSH connection, so handleBackup's
// dial-then-resolve-then-backup sequencing is testable. Close happens in a
// goroutine handleBackup spawns, concurrently with the test goroutine, so
// closed is reported through a channel rather than a plain bool the test
// would have to read unsynchronized.
type fakeBackupHost struct {
	target   orchestrate.Target
	closedCh chan struct{}
}

func newFakeBackupHost() *fakeBackupHost {
	return &fakeBackupHost{
		target:   orchestrate.Target{User: "git", Host: "forge.example.com", Port: "22"},
		closedCh: make(chan struct{}),
	}
}

func (f *fakeBackupHost) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	return nil
}
func (f *fakeBackupHost) Target() orchestrate.Target { return f.target }
func (f *fakeBackupHost) Close() error               { close(f.closedCh); return nil }

func (f *fakeBackupHost) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-f.closedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("host was not closed in time")
	}
}

// fakeAgeKeystore resolves initialize.KeyAgeBackupKey to a real,
// freshly-generated identity, and every other name to an empty secret —
// enough for backup.ResolveIdentity to succeed without a real keystore.
type fakeAgeKeystore struct {
	identity string
}

func newFakeAgeKeystore(t *testing.T) fakeAgeKeystore {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	return fakeAgeKeystore{identity: identity.String()}
}

func (f fakeAgeKeystore) Resolve(ctx context.Context, keyName string) (keystore.Secret, error) {
	if keyName == initialize.KeyAgeBackupKey {
		return keystore.NewSecret(f.identity), nil
	}
	return keystore.Secret{}, nil
}

func doBackup(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/backup", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandleBackupBadJSON(t *testing.T) {
	s := newTestServer()
	rec := doBackup(t, s, "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleBackupMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing bundleDir", `{"target":"ssh://user@host","to":"/tmp/dest"}`},
		{"missing target", `{"bundleDir":"/tmp/bundle","to":"/tmp/dest"}`},
		{"missing to", `{"bundleDir":"/tmp/bundle","target":"ssh://user@host"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer()
			rec := doBackup(t, s, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestHandleBackupInvalidTimeout(t *testing.T) {
	s := newTestServer()
	rec := doBackup(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"/tmp/dest","sshTimeout":"not-a-duration"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleBackupBundleLoadError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) {
		return nil, errors.New("no such bundle")
	}
	rec := doBackup(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"/tmp/dest"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleBackupDialFailureFailsJob(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dialSSH = func(ctx context.Context, target string, opts orchestrate.Options) (backupHost, error) {
		return nil, errors.New("connection refused")
	}
	s.backupRun = func(ctx context.Context, job *events.Job, opts backup.Options) error {
		t.Fatal("backupRun should not be called when dial fails")
		return nil
	}

	rec := doBackup(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"/tmp/dest"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	evs := job.Events()
	last := evs[len(evs)-1]
	if last.State != events.StateFailed {
		t.Errorf("last event state = %q, want failed", last.State)
	}
}

func TestHandleBackupKeystoreBuildError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	host := newFakeBackupHost()
	s.dialSSH = func(ctx context.Context, target string, opts orchestrate.Options) (backupHost, error) {
		return host, nil
	}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return nil, errors.New("bad driver config")
	}

	rec := doBackup(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"/tmp/dest"}`)
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	assertLastEventFailed(t, job)
	host.waitClosed(t)
}

func TestHandleBackupBlobBuildError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dialSSH = func(ctx context.Context, target string, opts orchestrate.Options) (backupHost, error) {
		return newFakeBackupHost(), nil
	}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return newFakeAgeKeystore(t), nil
	}
	s.newBlob = func(driverName string, config map[string]any) (blob.Adapter, error) {
		return nil, errors.New("bad blob driver config")
	}

	rec := doBackup(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"/tmp/dest"}`)
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	assertLastEventFailed(t, job)
}

func TestHandleBackupIdentityResolveError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dialSSH = func(ctx context.Context, target string, opts orchestrate.Options) (backupHost, error) {
		return newFakeBackupHost(), nil
	}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return fakeKeystoreDriver{}, nil // Resolve always returns an empty secret; not a parseable age identity.
	}
	s.newBlob = func(driverName string, config map[string]any) (blob.Adapter, error) {
		return blob.NewLocal(t.TempDir())
	}

	rec := doBackup(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"/tmp/dest"}`)
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	assertLastEventFailed(t, job)
}

func TestHandleBackupSuccessWiresOptions(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) {
		if dir != "/tmp/bundle" {
			t.Errorf("loadBundle dir = %q, want /tmp/bundle", dir)
		}
		return testBundle(), nil
	}
	host := newFakeBackupHost()
	var gotTarget string
	s.dialSSH = func(ctx context.Context, target string, opts orchestrate.Options) (backupHost, error) {
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
	var gotOpts backup.Options
	s.backupRun = func(ctx context.Context, job *events.Job, opts backup.Options) error {
		gotOpts = opts
		job.Succeeded("backup written to 20260101T000000Z.age")
		return nil
	}

	rec := doBackup(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"s3://bucket?endpoint=s3.example.com","remoteDir":"/srv/farrier"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	if !job.Done() {
		t.Fatal("job did not finish")
	}
	evs := job.Events()
	if last := evs[len(evs)-1]; last.State != events.StateSucceeded {
		t.Fatalf("last event = %+v, want succeeded", last)
	}

	if gotTarget != "ssh://user@host" {
		t.Errorf("target = %q, want ssh://user@host", gotTarget)
	}
	if gotOpts.Destination != "s3://bucket?endpoint=s3.example.com" {
		t.Errorf("Destination = %q, want the request's to", gotOpts.Destination)
	}
	if gotOpts.ForgejoVersion != testBundle().Manifest.Images["forgejo"] {
		t.Errorf("ForgejoVersion = %q, want the bundle's pinned forgejo image", gotOpts.ForgejoVersion)
	}
	if gotOpts.WorkDir == "" {
		t.Error("WorkDir is empty, want a generated scratch directory")
	}
	if gotOpts.Identity == nil {
		t.Error("Identity is nil, want the resolved age backup key")
	}
	if gotOpts.Git == nil || gotOpts.GitCapturer == nil || gotOpts.Database == nil || gotOpts.Blobs == nil || gotOpts.Keys == nil || gotOpts.PushHold == nil {
		t.Errorf("Options is missing an exporter or push hold: %+v", gotOpts)
	}
	host.waitClosed(t)
}

func TestHandleBackupDefaultsRemoteDir(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dialSSH = func(ctx context.Context, target string, opts orchestrate.Options) (backupHost, error) {
		return newFakeBackupHost(), nil
	}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return newFakeAgeKeystore(t), nil
	}
	s.newBlob = func(driverName string, config map[string]any) (blob.Adapter, error) {
		return blob.NewLocal(t.TempDir())
	}
	var ranBackup bool
	s.backupRun = func(ctx context.Context, job *events.Job, opts backup.Options) error {
		ranBackup = true
		job.Succeeded("done")
		return nil
	}

	rec := doBackup(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"/tmp/dest"}`)
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	if !ranBackup {
		t.Error("backupRun was not called")
	}
}

func jobFromResponse(t *testing.T, s *Server, rec *httptest.ResponseRecorder) *events.Job {
	t.Helper()
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
	return job
}

func assertLastEventFailed(t *testing.T, job *events.Job) {
	t.Helper()
	evs := job.Events()
	last := evs[len(evs)-1]
	if last.State != events.StateFailed {
		t.Errorf("last event state = %q, want failed", last.State)
	}
}
