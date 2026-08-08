package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/status"
)

// fakeKeystoreDriver satisfies keystore.Driver without a real driver
// build, so handleStatus's keystore-then-dial-then-check sequencing is
// testable.
type fakeKeystoreDriver struct{}

func (fakeKeystoreDriver) Resolve(ctx context.Context, keyName string) (keystore.Secret, error) {
	return keystore.Secret{}, nil
}

func doStatus(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/status?"+query, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandleStatusMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{"missing bundleDir", "target=ssh://user@host"},
		{"missing target", "bundleDir=/tmp/bundle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer()
			rec := doStatus(t, s, tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestHandleStatusInvalidTimeout(t *testing.T) {
	s := newTestServer()
	rec := doStatus(t, s, "bundleDir=/tmp/bundle&target=ssh://user@host&sshTimeout=not-a-duration")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleStatusBundleLoadError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) {
		return nil, errors.New("no such bundle")
	}
	rec := doStatus(t, s, "bundleDir=/tmp/bundle&target=ssh://user@host")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleStatusKeystoreBuildError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return nil, errors.New("bad driver config")
	}
	rec := doStatus(t, s, "bundleDir=/tmp/bundle&target=ssh://user@host")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandleStatusDialFailure(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return fakeKeystoreDriver{}, nil
	}
	s.dial = func(ctx context.Context, target string, opts orchestrate.Options) (Host, error) {
		return nil, errors.New("connection refused")
	}
	s.statusCheck = func(ctx context.Context, opts status.Options) (status.Report, error) {
		t.Fatal("statusCheck should not be called when dial fails")
		return status.Report{}, nil
	}

	rec := doStatus(t, s, "bundleDir=/tmp/bundle&target=ssh://user@host")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestHandleStatusSuccess(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) {
		if dir != "/tmp/bundle" {
			t.Errorf("loadBundle dir = %q, want /tmp/bundle", dir)
		}
		return testBundle(), nil
	}
	driver := fakeKeystoreDriver{}
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		if driverName != "file" {
			t.Errorf("keystore driverName = %q, want file", driverName)
		}
		return driver, nil
	}
	host := newFakeHost()
	var gotTarget string
	s.dial = func(ctx context.Context, target string, opts orchestrate.Options) (Host, error) {
		gotTarget = target
		return host, nil
	}
	notAfter := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	var gotOpts status.Options
	s.statusCheck = func(ctx context.Context, opts status.Options) (status.Report, error) {
		gotOpts = opts
		return status.Report{
			Services: []status.ServiceStatus{{Name: "forge", Up: true, Detail: "Up 3 hours"}},
			TLS:      status.TLSStatus{NotAfter: notAfter, Valid: true},
			Disk:     status.DiskStatus{Path: "/", TotalBytes: 100, UsedBytes: 40, AvailableBytes: 60},
			Lag:      status.Lag{State: status.LagUnmeasured},
		}, nil
	}

	rec := doStatus(t, s, "bundleDir=/tmp/bundle&target=ssh://user@host&remoteDir=/srv/farrier")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if gotTarget != "ssh://user@host" {
		t.Errorf("target = %q, want ssh://user@host", gotTarget)
	}
	if gotOpts.RemoteDir != "/srv/farrier" {
		t.Errorf("RemoteDir = %q, want /srv/farrier", gotOpts.RemoteDir)
	}
	if gotOpts.DiskPath != status.DefaultDiskPath {
		t.Errorf("DiskPath = %q, want %q (default)", gotOpts.DiskPath, status.DefaultDiskPath)
	}
	if gotOpts.Runner != host {
		t.Error("Runner was not the dialed host")
	}
	host.waitClosed(t)

	var resp statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Services) != 1 || resp.Services[0].Name != "forge" || !resp.Services[0].Up {
		t.Errorf("Services = %+v", resp.Services)
	}
	if !resp.TLS.Valid || !resp.TLS.NotAfter.Equal(notAfter) {
		t.Errorf("TLS = %+v", resp.TLS)
	}
	if resp.Disk.AvailableBytes != 60 {
		t.Errorf("Disk.AvailableBytes = %d, want 60", resp.Disk.AvailableBytes)
	}
	if resp.Lag.State != status.LagUnmeasured {
		t.Errorf("Lag.State = %q, want unmeasured", resp.Lag.State)
	}
}

func TestHandleStatusDefaultsRemoteDirAndDiskPath(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return fakeKeystoreDriver{}, nil
	}
	s.dial = func(ctx context.Context, target string, opts orchestrate.Options) (Host, error) {
		return newFakeHost(), nil
	}
	var gotOpts status.Options
	s.statusCheck = func(ctx context.Context, opts status.Options) (status.Report, error) {
		gotOpts = opts
		return status.Report{}, nil
	}

	rec := doStatus(t, s, "bundleDir=/tmp/bundle&target=ssh://user@host")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if gotOpts.RemoteDir != defaultRemoteDir {
		t.Errorf("RemoteDir = %q, want %q", gotOpts.RemoteDir, defaultRemoteDir)
	}
	if gotOpts.DiskPath != status.DefaultDiskPath {
		t.Errorf("DiskPath = %q, want %q", gotOpts.DiskPath, status.DefaultDiskPath)
	}
}

func TestHandleStatusCheckError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.newKeystore = func(driverName string, config map[string]any) (keystore.Driver, error) {
		return fakeKeystoreDriver{}, nil
	}
	s.dial = func(ctx context.Context, target string, opts orchestrate.Options) (Host, error) {
		return newFakeHost(), nil
	}
	s.statusCheck = func(ctx context.Context, opts status.Options) (status.Report, error) {
		return status.Report{}, errors.New("df: command not found")
	}

	rec := doStatus(t, s, "bundleDir=/tmp/bundle&target=ssh://user@host")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}
