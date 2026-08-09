package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/restore"
)

// fakeRestoreHost implements restore.Host (deploy.Host + RunStdin) without
// a real SSH connection, so handleRestore's dial-then-resolve-then-restore
// sequencing is testable. Close happens in a goroutine handleRestore
// spawns, concurrently with the test goroutine — the same closedCh
// synchronization fakeHost (up_test.go) and fakeBackupHost (backup_test.go)
// use for the same reason.
type fakeRestoreHost struct {
	closedCh chan struct{}
}

func newFakeRestoreHost() *fakeRestoreHost {
	return &fakeRestoreHost{closedCh: make(chan struct{})}
}

func (f *fakeRestoreHost) Output(ctx context.Context, command string) ([]byte, error) {
	return nil, nil
}
func (f *fakeRestoreHost) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	return nil
}
func (f *fakeRestoreHost) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	return nil
}
func (f *fakeRestoreHost) CheckHost(ctx context.Context) error { return nil }
func (f *fakeRestoreHost) Close() error                        { close(f.closedCh); return nil }
func (f *fakeRestoreHost) RunStdin(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	io.Copy(io.Discard, stdin)
	return nil
}

func (f *fakeRestoreHost) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-f.closedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("host was not closed in time")
	}
}

func doRestore(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/restore", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandleRestoreBadJSON(t *testing.T) {
	s := newTestServer()
	rec := doRestore(t, s, "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleRestoreMissingRequiredFields(t *testing.T) {
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
			rec := doRestore(t, s, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestHandleRestoreInvalidTimeout(t *testing.T) {
	s := newTestServer()
	rec := doRestore(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":"/tmp/src","sshTimeout":"not-a-duration"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleRestoreBundleLoadError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) {
		return nil, errors.New("no such bundle")
	}
	rec := doRestore(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":"/tmp/src"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleRestoreDialFailureFailsJob(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dialRestore = func(ctx context.Context, target string, opts orchestrate.Options) (restore.Host, error) {
		return nil, errors.New("connection refused")
	}
	s.restoreRun = func(ctx context.Context, job *events.Job, opts restore.Options) error {
		t.Fatal("restoreRun should not be called when dial fails")
		return nil
	}

	rec := doRestore(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":"/tmp/src"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	assertLastEventFailed(t, job)
}

func TestHandleRestoreKeystoreBuildError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	host := newFakeRestoreHost()
	s.dialRestore = func(ctx context.Context, target string, opts orchestrate.Options) (restore.Host, error) {
		return host, nil
	}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return nil, errors.New("bad driver config")
	}

	rec := doRestore(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":"/tmp/src"}`)
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	assertLastEventFailed(t, job)
	host.waitClosed(t)
}

func TestHandleRestoreSuccessWiresOptions(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) {
		if dir != "/tmp/bundle" {
			t.Errorf("loadBundle dir = %q, want /tmp/bundle", dir)
		}
		return testBundle(), nil
	}
	host := newFakeRestoreHost()
	var gotTarget string
	s.dialRestore = func(ctx context.Context, target string, opts orchestrate.Options) (restore.Host, error) {
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
	var gotOpts restore.Options
	s.restoreRun = func(ctx context.Context, job *events.Job, opts restore.Options) error {
		gotOpts = opts
		job.Succeeded("instance restored")
		return nil
	}

	from := t.TempDir()
	rec := doRestore(t, s, fmt.Sprintf(`{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":%q,"snapshot":"20260101T000000Z.age","remoteDir":"/srv/farrier"}`, from))
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
	if gotOpts.WorkDir == "" {
		t.Error("WorkDir is empty, want a generated scratch directory")
	}
	if gotOpts.Identity == nil {
		t.Error("Identity is nil, want the resolved age backup key")
	}
	if gotOpts.Source == nil || gotOpts.Blobs == nil || gotOpts.Keystore == nil || gotOpts.Host == nil || gotOpts.Bundle == nil {
		t.Errorf("Options is missing a required field: %+v", gotOpts)
	}
	host.waitClosed(t)
}

func TestHandleRestoreDefaultsRemoteDir(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dialRestore = func(ctx context.Context, target string, opts orchestrate.Options) (restore.Host, error) {
		return newFakeRestoreHost(), nil
	}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return newFakeAgeKeystore(t), nil
	}
	s.newBlob = func(driverName string, config map[string]any) (blob.Adapter, error) {
		return blob.NewLocal(t.TempDir())
	}
	var gotOpts restore.Options
	s.restoreRun = func(ctx context.Context, job *events.Job, opts restore.Options) error {
		gotOpts = opts
		job.Succeeded("done")
		return nil
	}

	rec := doRestore(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":"/tmp/src"}`)
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	if gotOpts.RemoteDir != defaultRemoteDir {
		t.Errorf("RemoteDir = %q, want default %q", gotOpts.RemoteDir, defaultRemoteDir)
	}
}
