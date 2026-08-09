package api

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/evandelacruz/farrier/internal/core/upgrade"
)

// fakeUpgradeHost implements upgradeHost (upgrade.Host + Close) without a
// real SSH connection, so handleUpgrade's dial-then-resolve-then-upgrade
// sequencing is testable. Close happens in a goroutine handleUpgrade
// spawns, concurrently with the test goroutine, so closed is reported
// through a channel rather than a plain bool the test would have to read
// unsynchronized.
type fakeUpgradeHost struct {
	target   orchestrate.Target
	closedCh chan struct{}
}

func newFakeUpgradeHost() *fakeUpgradeHost {
	return &fakeUpgradeHost{
		target:   orchestrate.Target{User: "git", Host: "forge.example.com", Port: "22"},
		closedCh: make(chan struct{}),
	}
}

func (f *fakeUpgradeHost) Output(ctx context.Context, command string) ([]byte, error) {
	return nil, nil
}
func (f *fakeUpgradeHost) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	return nil
}
func (f *fakeUpgradeHost) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	return nil
}
func (f *fakeUpgradeHost) CheckHost(ctx context.Context) error { return nil }
func (f *fakeUpgradeHost) Target() orchestrate.Target          { return f.target }
func (f *fakeUpgradeHost) Close() error                        { close(f.closedCh); return nil }

func (f *fakeUpgradeHost) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-f.closedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("host was not closed in time")
	}
}

func doUpgrade(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/upgrade", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandleUpgradeBadJSON(t *testing.T) {
	s := newTestServer()
	rec := doUpgrade(t, s, "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleUpgradeMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing bundleDir", `{"target":"ssh://user@host","to":"/tmp/dest","image":"forgejo:1.22"}`},
		{"missing target", `{"bundleDir":"/tmp/bundle","to":"/tmp/dest","image":"forgejo:1.22"}`},
		{"missing to", `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","image":"forgejo:1.22"}`},
		{"missing image", `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"/tmp/dest"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer()
			rec := doUpgrade(t, s, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestHandleUpgradeInvalidTimeout(t *testing.T) {
	s := newTestServer()
	rec := doUpgrade(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"/tmp/dest","image":"forgejo:1.22","sshTimeout":"not-a-duration"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleUpgradeBundleLoadError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) {
		return nil, errors.New("no such bundle")
	}
	rec := doUpgrade(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"/tmp/dest","image":"forgejo:1.22"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleUpgradeDialFailureFailsJob(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dialUpgrade = func(ctx context.Context, target string, opts orchestrate.Options) (upgradeHost, error) {
		return nil, errors.New("connection refused")
	}
	s.upgradeRun = func(ctx context.Context, job *events.Job, opts upgrade.Options) error {
		t.Fatal("upgradeRun should not be called when dial fails")
		return nil
	}

	rec := doUpgrade(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"/tmp/dest","image":"forgejo:1.22"}`)
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	assertLastEventFailed(t, job)
}

func TestHandleUpgradeKeystoreBuildError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	host := newFakeUpgradeHost()
	s.dialUpgrade = func(ctx context.Context, target string, opts orchestrate.Options) (upgradeHost, error) {
		return host, nil
	}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return nil, errors.New("bad driver config")
	}

	rec := doUpgrade(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"/tmp/dest","image":"forgejo:1.22"}`)
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	assertLastEventFailed(t, job)
	host.waitClosed(t)
}

func TestHandleUpgradeBlobBuildError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dialUpgrade = func(ctx context.Context, target string, opts orchestrate.Options) (upgradeHost, error) {
		return newFakeUpgradeHost(), nil
	}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return newFakeAgeKeystore(t), nil
	}
	s.newBlob = func(driverName string, config map[string]any) (blob.Adapter, error) {
		return nil, errors.New("bad blob driver config")
	}

	rec := doUpgrade(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"/tmp/dest","image":"forgejo:1.22"}`)
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	assertLastEventFailed(t, job)
}

func TestHandleUpgradeIdentityResolveError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dialUpgrade = func(ctx context.Context, target string, opts orchestrate.Options) (upgradeHost, error) {
		return newFakeUpgradeHost(), nil
	}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return fakeKeystoreDriver{}, nil // Resolve always returns an empty secret; not a parseable age identity.
	}
	s.newBlob = func(driverName string, config map[string]any) (blob.Adapter, error) {
		return blob.NewLocal(t.TempDir())
	}

	rec := doUpgrade(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"/tmp/dest","image":"forgejo:1.22"}`)
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	assertLastEventFailed(t, job)
}

func TestHandleUpgradeSuccessWiresOptions(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) {
		if dir != "/tmp/bundle" {
			t.Errorf("loadBundle dir = %q, want /tmp/bundle", dir)
		}
		return testBundle(), nil
	}
	host := newFakeUpgradeHost()
	var gotTarget string
	s.dialUpgrade = func(ctx context.Context, target string, opts orchestrate.Options) (upgradeHost, error) {
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
	var gotOpts upgrade.Options
	s.upgradeRun = func(ctx context.Context, job *events.Job, opts upgrade.Options) error {
		gotOpts = opts
		job.Succeeded("upgraded to forgejo test")
		return nil
	}

	rec := doUpgrade(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"s3://bucket?endpoint=s3.example.com","image":"codeberg.org/forgejo/forgejo:1.22","remoteDir":"/srv/farrier"}`)
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
	if gotOpts.BundleDir != "/tmp/bundle" {
		t.Errorf("BundleDir = %q, want /tmp/bundle", gotOpts.BundleDir)
	}
	if gotOpts.RemoteDir != "/srv/farrier" {
		t.Errorf("RemoteDir = %q, want /srv/farrier", gotOpts.RemoteDir)
	}
	if gotOpts.Destination != "s3://bucket?endpoint=s3.example.com" {
		t.Errorf("Destination = %q, want the request's to", gotOpts.Destination)
	}
	if gotOpts.NewImage != "codeberg.org/forgejo/forgejo:1.22" {
		t.Errorf("NewImage = %q, want the request's image", gotOpts.NewImage)
	}
	if gotOpts.WorkDir == "" {
		t.Error("WorkDir is empty, want a generated scratch directory")
	}
	if gotOpts.Identity == nil {
		t.Error("Identity is nil, want the resolved age backup key")
	}
	host.waitClosed(t)
}

func TestHandleUpgradeDefaultsRemoteDir(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dialUpgrade = func(ctx context.Context, target string, opts orchestrate.Options) (upgradeHost, error) {
		return newFakeUpgradeHost(), nil
	}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return newFakeAgeKeystore(t), nil
	}
	s.newBlob = func(driverName string, config map[string]any) (blob.Adapter, error) {
		return blob.NewLocal(t.TempDir())
	}
	var gotOpts upgrade.Options
	s.upgradeRun = func(ctx context.Context, job *events.Job, opts upgrade.Options) error {
		gotOpts = opts
		job.Succeeded("done")
		return nil
	}

	rec := doUpgrade(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","to":"/tmp/dest","image":"forgejo:1.22"}`)
	job := jobFromResponse(t, s, rec)
	waitDone(t, job)
	if gotOpts.RemoteDir != defaultRemoteDir {
		t.Errorf("RemoteDir = %q, want default %q", gotOpts.RemoteDir, defaultRemoteDir)
	}
}
