package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/importer"
)

func doImport(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/import", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandleImportBadJSON(t *testing.T) {
	s := newTestServer()
	rec := doImport(t, s, "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleImportMissingTarget(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	t.Setenv("FARRIER_SOURCE_TOKEN", "s")
	s := newTestServer()
	rec := doImport(t, s, `{"source":"https://github.com/acme/widgets","owner":"acme"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleImportSourceAndReposMutuallyExclusive(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	t.Setenv("FARRIER_SOURCE_TOKEN", "s")
	s := newTestServer()

	cases := []string{
		`{"target":"https://git.example.com","owner":"acme"}`,
		`{"target":"https://git.example.com","owner":"acme","source":"https://github.com/acme/widgets","repos":[{"source":"https://github.com/acme/other"}]}`,
	}
	for _, body := range cases {
		rec := doImport(t, s, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestHandleImportMissingTokens(t *testing.T) {
	cases := []struct {
		name        string
		targetToken string
		sourceToken string
	}{
		{"missing target token", "", "s"},
		{"missing source token", "t", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.targetToken != "" {
				t.Setenv("FARRIER_TARGET_TOKEN", tc.targetToken)
			}
			if tc.sourceToken != "" {
				t.Setenv("FARRIER_SOURCE_TOKEN", tc.sourceToken)
			}
			s := newTestServer()
			rec := doImport(t, s, `{"target":"https://git.example.com","source":"https://github.com/acme/widgets","owner":"acme"}`)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
			}
		})
	}
}

func TestHandleImportSingleSourceStartsJob(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "target-token")
	t.Setenv("FARRIER_SOURCE_TOKEN", "source-token")
	s := newTestServer()

	var gotOpts importer.Options
	done := make(chan struct{})
	s.importRun = func(ctx context.Context, job *events.Job, opts importer.Options) (importer.Result, error) {
		gotOpts = opts
		job.Succeeded("imported")
		close(done)
		return importer.Result{}, nil
	}
	s.importRunBatch = func(ctx context.Context, job *events.Job, opts importer.BatchOptions) (importer.BatchResult, error) {
		t.Fatal("importRunBatch should not be called for a single-source request")
		return importer.BatchResult{}, nil
	}

	rec := doImport(t, s, `{
		"target": "https://git.example.com",
		"source": "https://github.com/acme/widgets",
		"owner": "acme",
		"name": "widgets",
		"mirror": true,
		"mirrorInterval": "1h"
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

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("importRun was not called")
	}

	if gotOpts.TargetBaseURL != "https://git.example.com" {
		t.Errorf("TargetBaseURL = %q", gotOpts.TargetBaseURL)
	}
	if gotOpts.SourceURL != "https://github.com/acme/widgets" {
		t.Errorf("SourceURL = %q", gotOpts.SourceURL)
	}
	if gotOpts.RepoOwner != "acme" {
		t.Errorf("RepoOwner = %q, want acme", gotOpts.RepoOwner)
	}
	if gotOpts.RepoName != "widgets" {
		t.Errorf("RepoName = %q, want widgets", gotOpts.RepoName)
	}
	if gotOpts.TargetToken.Reveal() != "target-token" {
		t.Errorf("TargetToken = %q", gotOpts.TargetToken.Reveal())
	}
	if gotOpts.SourceToken.Reveal() != "source-token" {
		t.Errorf("SourceToken = %q", gotOpts.SourceToken.Reveal())
	}
	if !gotOpts.Private {
		t.Error("Private = false, want true (default)")
	}
	if !gotOpts.Mirror {
		t.Error("Mirror = false, want true")
	}
	if gotOpts.MirrorInterval != time.Hour {
		t.Errorf("MirrorInterval = %v, want 1h", gotOpts.MirrorInterval)
	}

	job, ok := s.jobs.Get(resp.JobID)
	if !ok {
		t.Fatalf("job %q not found", resp.JobID)
	}
	if !job.Done() {
		t.Fatal("job not terminal after importRun returned")
	}
}

func TestHandleImportDefaultsPrivateTrueMirrorFalse(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	t.Setenv("FARRIER_SOURCE_TOKEN", "s")
	s := newTestServer()

	var gotOpts importer.Options
	done := make(chan struct{})
	s.importRun = func(ctx context.Context, job *events.Job, opts importer.Options) (importer.Result, error) {
		gotOpts = opts
		job.Succeeded("imported")
		close(done)
		return importer.Result{}, nil
	}

	rec := doImport(t, s, `{"target":"https://git.example.com","source":"https://github.com/acme/widgets","owner":"acme"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	<-done

	if !gotOpts.Private {
		t.Error("Private = false, want true (default)")
	}
	if gotOpts.Mirror {
		t.Error("Mirror = true, want false (default)")
	}
	if gotOpts.MirrorInterval != 0 {
		t.Errorf("MirrorInterval = %v, want 0", gotOpts.MirrorInterval)
	}
}

func TestHandleImportInvalidMirrorInterval(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	t.Setenv("FARRIER_SOURCE_TOKEN", "s")
	s := newTestServer()
	rec := doImport(t, s, `{
		"target": "https://git.example.com",
		"source": "https://github.com/acme/widgets",
		"owner": "acme",
		"mirror": true,
		"mirrorInterval": "not-a-duration"
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleImportRepoMissingOwner(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	t.Setenv("FARRIER_SOURCE_TOKEN", "s")
	s := newTestServer()
	rec := doImport(t, s, `{"target":"https://git.example.com","repos":[{"source":"https://github.com/acme/widgets"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestHandleImportBatchStartsJob(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	t.Setenv("FARRIER_SOURCE_TOKEN", "s")
	s := newTestServer()

	var gotOpts importer.BatchOptions
	done := make(chan struct{})
	s.importRunBatch = func(ctx context.Context, job *events.Job, opts importer.BatchOptions) (importer.BatchResult, error) {
		gotOpts = opts
		job.Succeeded("imported")
		close(done)
		return importer.BatchResult{}, nil
	}
	s.importRun = func(ctx context.Context, job *events.Job, opts importer.Options) (importer.Result, error) {
		t.Fatal("importRun should not be called for a batch request")
		return importer.Result{}, nil
	}

	rec := doImport(t, s, `{
		"target": "https://git.example.com",
		"owner": "acme",
		"repos": [
			{"source": "https://github.com/acme/widgets"},
			{"source": "https://github.com/acme/gadgets", "owner": "other", "private": false}
		]
	}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("importRunBatch was not called")
	}

	if len(gotOpts.Repos) != 2 {
		t.Fatalf("Repos = %d, want 2", len(gotOpts.Repos))
	}
	if gotOpts.Repos[0].RepoOwner != "acme" {
		t.Errorf("Repos[0].RepoOwner = %q, want acme (batch default)", gotOpts.Repos[0].RepoOwner)
	}
	if !gotOpts.Repos[0].Private {
		t.Error("Repos[0].Private = false, want true (default)")
	}
	if gotOpts.Repos[1].RepoOwner != "other" {
		t.Errorf("Repos[1].RepoOwner = %q, want other (entry override)", gotOpts.Repos[1].RepoOwner)
	}
	if gotOpts.Repos[1].Private {
		t.Error("Repos[1].Private = true, want false (entry override)")
	}
}
