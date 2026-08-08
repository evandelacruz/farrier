package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/initialize"
)

func newTestServer() *Server {
	return &Server{jobs: events.NewStore()}
}

func doInit(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/init", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandleInitBadJSON(t *testing.T) {
	s := newTestServer()
	rec := doInit(t, s, "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleInitMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing domain", `{"keystore":{"driver":"file"},"blob":{"driver":"local"},"acmeDnsProvider":"manual"}`},
		{"missing keystore driver", `{"domain":"example.com","blob":{"driver":"local"},"acmeDnsProvider":"manual"}`},
		{"missing blob driver", `{"domain":"example.com","keystore":{"driver":"file"},"acmeDnsProvider":"manual"}`},
		{"missing acme dns provider", `{"domain":"example.com","keystore":{"driver":"file"},"blob":{"driver":"local"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer()
			rec := doInit(t, s, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestHandleInitStartsJobAndReturnsID(t *testing.T) {
	s := newTestServer()

	var gotParams initialize.Params
	done := make(chan struct{})
	s.initRun = func(ctx context.Context, job *events.Job, params initialize.Params) (*bundle.Bundle, error) {
		gotParams = params
		job.Succeeded("done")
		close(done)
		return nil, nil
	}

	rec := doInit(t, s, `{
		"domain": "example.com",
		"dir": "/tmp/bundle",
		"keystore": {"driver": "file", "config": {"path": "/tmp/keys"}},
		"blob": {"driver": "local", "config": {"path": "/tmp/blobs"}},
		"acmeDnsProvider": "manual",
		"acmeEmail": "ops@example.com",
		"images": {"forgejo": "codeberg.org/forgejo/forgejo:latest"}
	}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var resp struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.JobID == "" {
		t.Fatalf("response carried no jobId: %s", rec.Body.String())
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("initRun was not called")
	}

	if gotParams.Domain != "example.com" {
		t.Errorf("Domain = %q, want example.com", gotParams.Domain)
	}
	if gotParams.Dir != "/tmp/bundle" {
		t.Errorf("Dir = %q, want /tmp/bundle", gotParams.Dir)
	}
	if gotParams.Keystore.Driver != "file" || gotParams.Keystore.Config["path"] != "/tmp/keys" {
		t.Errorf("Keystore = %+v", gotParams.Keystore)
	}
	if gotParams.Blob.Driver != "local" {
		t.Errorf("Blob = %+v", gotParams.Blob)
	}
	if gotParams.ACMEDNSProvider != "manual" || gotParams.ACMEEmail != "ops@example.com" {
		t.Errorf("ACMEDNSProvider/ACMEEmail = %q/%q", gotParams.ACMEDNSProvider, gotParams.ACMEEmail)
	}
	if gotParams.Images["forgejo"] != "codeberg.org/forgejo/forgejo:latest" {
		t.Errorf("Images = %+v", gotParams.Images)
	}

	job, ok := s.jobs.Get(resp.JobID)
	if !ok {
		t.Fatalf("job %q not found in store", resp.JobID)
	}
	if !job.Done() {
		t.Fatalf("job not terminal after initRun returned")
	}
}

func TestHandleInitDefaultsDir(t *testing.T) {
	s := newTestServer()
	var gotDir string
	done := make(chan struct{})
	s.initRun = func(ctx context.Context, job *events.Job, params initialize.Params) (*bundle.Bundle, error) {
		gotDir = params.Dir
		job.Succeeded("done")
		close(done)
		return nil, nil
	}

	rec := doInit(t, s, `{"domain":"example.com","keystore":{"driver":"file"},"blob":{"driver":"local"},"acmeDnsProvider":"manual"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	<-done
	if gotDir != "." {
		t.Errorf("Dir = %q, want \".\"", gotDir)
	}
}
