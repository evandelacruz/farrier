package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/attach"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

func doAttach(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/attach", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandleAttachRejectsMalformedRequests(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"bad json", "{not json"},
		{"missing bundleDir", `{"target":"ssh://user@host"}`},
		{"missing target", `{"bundleDir":"/tmp/bundle"}`},
		{"invalid timeout", `{"bundleDir":"/tmp/bundle","target":"ssh://user@host","sshTimeout":"nope"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer()
			if rec := doAttach(t, s, tc.body); rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestHandleAttachBundleLoadError(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return nil, errors.New("no such bundle") }
	if rec := doAttach(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleAttachDialFailureFailsJob(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dial = func(ctx context.Context, target string, opts orchestrate.Options) (Host, error) {
		return nil, errors.New("connection refused")
	}
	s.attachRun = func(ctx context.Context, job *events.Job, opts attach.Options) (*bundle.Bundle, error) {
		t.Fatal("attachRun should not be called when dial fails")
		return nil, nil
	}

	rec := doAttach(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	job := jobFrom(t, s, rec)
	waitDone(t, job)

	evs := job.Events()
	if last := evs[len(evs)-1]; last.State != events.StateFailed {
		t.Errorf("last event state = %q, want failed", last.State)
	}
}

// UP-007 through the second frontend: every input the operator supplies
// reaches the core unchanged, and the core is what decides whether the
// bundle may be named at all.
func TestHandleAttachPassesEveryInputThrough(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	host := newFakeHost()
	s.dial = func(ctx context.Context, target string, opts orchestrate.Options) (Host, error) {
		return host, nil
	}
	got := make(chan attach.Options, 1)
	s.attachRun = func(ctx context.Context, job *events.Job, opts attach.Options) (*bundle.Bundle, error) {
		got <- opts
		job.Succeeded("named")
		return nil, nil
	}

	rec := doAttach(t, s, `{
		"bundleDir":"/tmp/bundle",
		"target":"ssh://user@host",
		"remoteDir":"/srv/farrier",
		"domain":"forge.example.com",
		"acmeDnsProvider":"cloudflare",
		"acmeEmail":"ops@example.com",
		"acmeDirectory":"staging",
		"address":"box.tail1234.ts.net"
	}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	select {
	case opts := <-got:
		if opts.Domain != "forge.example.com" {
			t.Errorf("Domain = %q", opts.Domain)
		}
		if opts.ACMEDNSProvider != "cloudflare" {
			t.Errorf("ACMEDNSProvider = %q", opts.ACMEDNSProvider)
		}
		if opts.ACMEEmail != "ops@example.com" {
			t.Errorf("ACMEEmail = %q", opts.ACMEEmail)
		}
		if opts.ACMEDirectory != "staging" {
			t.Errorf("ACMEDirectory = %q, want it carried through to the core", opts.ACMEDirectory)
		}
		if opts.Address != "box.tail1234.ts.net" {
			t.Errorf("Address = %q", opts.Address)
		}
		if opts.RemoteDir != "/srv/farrier" {
			t.Errorf("RemoteDir = %q", opts.RemoteDir)
		}
		if opts.BundleDir != "/tmp/bundle" {
			t.Errorf("BundleDir = %q", opts.BundleDir)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("attachRun was not called in time")
	}
	host.waitClosed(t)
}

func TestHandleAttachDefaultsRemoteDir(t *testing.T) {
	s := newTestServer()
	s.loadBundle = func(dir string) (*bundle.Bundle, error) { return testBundle(), nil }
	s.dial = func(ctx context.Context, target string, opts orchestrate.Options) (Host, error) {
		return newFakeHost(), nil
	}
	got := make(chan string, 1)
	s.attachRun = func(ctx context.Context, job *events.Job, opts attach.Options) (*bundle.Bundle, error) {
		got <- opts.RemoteDir
		job.Succeeded("named")
		return nil, nil
	}

	doAttach(t, s, `{"bundleDir":"/tmp/bundle","target":"ssh://user@host"}`)
	select {
	case remoteDir := <-got:
		if remoteDir != defaultRemoteDir {
			t.Errorf("remoteDir = %q, want %q", remoteDir, defaultRemoteDir)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("attachRun was not called in time")
	}
}

// jobFrom reads the job ID out of an accepted response and looks the job up.
func jobFrom(t *testing.T, s *Server, rec *httptest.ResponseRecorder) *events.Job {
	t.Helper()
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
