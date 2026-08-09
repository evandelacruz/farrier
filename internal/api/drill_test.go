package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/drill"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/restore"
)

// drill.Host is restore.Host, so fakeRestoreHost (restore_test.go) already
// satisfies everything a drill needs from a dialed scratch target.

func doDrill(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/drill", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandleDrillBadJSON(t *testing.T) {
	s := newTestServer()
	rec := doDrill(t, s, "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleDrillMissingRequiredFields(t *testing.T) {
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
			rec := doDrill(t, s, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestHandleDrillInvalidTimeout(t *testing.T) {
	s := newTestServer()
	rec := doDrill(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":"/tmp/src","sshTimeout":"not-a-duration"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleDrillBundleLoadError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) {
		return nil, errors.New("no such bundle")
	}
	rec := doDrill(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":"/tmp/src"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleDrillDialFailureFailsJob(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dialDrill = func(ctx context.Context, target string, opts orchestrate.Options) (drill.Host, error) {
		return nil, errors.New("connection refused")
	}
	s.drillRun = func(ctx context.Context, job *events.Job, opts drill.Options) (drill.Report, error) {
		t.Fatal("drillRun should not be called when dial fails")
		return drill.Report{}, nil
	}

	rec := doDrill(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":"/tmp/src"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	assertLastEventFailed(t, job)
}

func TestHandleDrillKeystoreBuildError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	host := newFakeRestoreHost()
	s.dialDrill = func(ctx context.Context, target string, opts orchestrate.Options) (drill.Host, error) {
		return host, nil
	}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return nil, errors.New("bad driver config")
	}

	rec := doDrill(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":"/tmp/src"}`)
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	assertLastEventFailed(t, job)
	host.waitClosed(t)
}

func TestHandleDrillSuccessWiresOptions(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) {
		if dir != "/tmp/bundle" {
			t.Errorf("loadBundle dir = %q, want /tmp/bundle", dir)
		}
		return testBundle(), nil
	}
	host := newFakeRestoreHost()
	var gotTarget string
	s.dialDrill = func(ctx context.Context, target string, opts orchestrate.Options) (drill.Host, error) {
		gotTarget = target
		return host, nil
	}
	ageKeystore := newFakeAgeKeystore(t)
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return ageKeystore, nil
	}
	s.newBlob = func(driverName string, config map[string]any) (blob.Adapter, error) {
		return blob.NewLocal(t.TempDir())
	}
	var gotOpts drill.Options
	s.drillRun = func(ctx context.Context, job *events.Job, opts drill.Options) (drill.Report, error) {
		gotOpts = opts
		job.Succeeded("drill succeeded")
		return drill.Report{SnapshotKey: "20260101T000000Z.age"}, nil
	}

	from := t.TempDir()
	rec := doDrill(t, s, fmt.Sprintf(`{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":%q,"remoteDir":"/srv/farrier-drill"}`, from))
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
	if gotOpts.RemoteDir != "/srv/farrier-drill" {
		t.Errorf("RemoteDir = %q, want /srv/farrier-drill", gotOpts.RemoteDir)
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

func TestHandleDrillDefaultsRemoteDir(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dialDrill = func(ctx context.Context, target string, opts orchestrate.Options) (drill.Host, error) {
		return newFakeRestoreHost(), nil
	}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return newFakeAgeKeystore(t), nil
	}
	s.newBlob = func(driverName string, config map[string]any) (blob.Adapter, error) {
		return blob.NewLocal(t.TempDir())
	}
	var gotOpts drill.Options
	s.drillRun = func(ctx context.Context, job *events.Job, opts drill.Options) (drill.Report, error) {
		gotOpts = opts
		job.Succeeded("done")
		return drill.Report{}, nil
	}

	rec := doDrill(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":"/tmp/src"}`)
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	if gotOpts.RemoteDir != defaultRemoteDir {
		t.Errorf("RemoteDir = %q, want default %q", gotOpts.RemoteDir, defaultRemoteDir)
	}
}

// TestHandleDrillFailureNamesTheStep is DRIL-001's report reaching the API
// frontend: the job's terminal event carries the specific failing step, so
// a dashboard renders the same thing the CLI prints.
func TestHandleDrillFailureNamesTheStep(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dialDrill = func(ctx context.Context, target string, opts orchestrate.Options) (drill.Host, error) {
		return newFakeRestoreHost(), nil
	}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return newFakeAgeKeystore(t), nil
	}
	s.newBlob = func(driverName string, config map[string]any) (blob.Adapter, error) {
		return blob.NewLocal(t.TempDir())
	}
	s.drillRun = func(ctx context.Context, job *events.Job, opts drill.Options) (drill.Report, error) {
		failure := &drill.Failure{Step: restore.StepVerify, Detail: "checksum mismatch for repos/acme-widgets.tar"}
		job.Failed(failure.Error())
		return drill.Report{Failure: failure}, failure
	}

	rec := doDrill(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","from":"/tmp/src"}`)
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)

	evs := job.Events()
	last := evs[len(evs)-1]
	if last.State != events.StateFailed {
		t.Fatalf("last event = %+v, want failed", last)
	}
	if !bytes.Contains([]byte(last.Detail), []byte(restore.StepVerify)) {
		t.Errorf("terminal event detail = %q, want it to name step %q", last.Detail, restore.StepVerify)
	}
}
