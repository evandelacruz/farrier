package importer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// fakeForgejo is a minimal stand-in for Forgejo's POST /api/v1/repos/migrate
// endpoint: enough to exercise Run's request shape and response handling.
type fakeForgejo struct {
	wantToken   string
	failStatus  int
	failMessage string
	lastReq     migrateRequest
	nextID      int64
}

func (f *fakeForgejo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1/repos/migrate" || r.Method != http.MethodPost {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if got := r.Header.Get("Authorization"); got != "token "+f.wantToken {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(apiError{Message: "invalid token"})
		return
	}

	var req migrateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.lastReq = req

	if f.failStatus != 0 {
		w.WriteHeader(f.failStatus)
		json.NewEncoder(w).Encode(apiError{Message: f.failMessage})
		return
	}

	f.nextID++
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(migrateResponse{
		ID:       f.nextID,
		FullName: req.RepoOwner + "/" + req.RepoName,
		HTMLURL:  "https://target.example.com/" + req.RepoOwner + "/" + req.RepoName,
	})
}

func newTestServer(t *testing.T, fake *fakeForgejo) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return server
}

func baseOptions(server *httptest.Server, token string) Options {
	return Options{
		TargetBaseURL: server.URL,
		TargetToken:   keystore.NewSecret(token),
		SourceURL:     "https://github.com/acme/widgets",
		SourceToken:   keystore.NewSecret("source-token"),
		RepoOwner:     "acme",
		Private:       true,
	}
}

func TestRunMigratesRepository(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token"}
	server := newTestServer(t, fake)

	job := events.NewJob()
	result, err := Run(context.Background(), job, baseOptions(server, "target-token"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.FullName != "acme/widgets" {
		t.Errorf("FullName = %q, want acme/widgets", result.FullName)
	}
	if result.Mirror {
		t.Errorf("Mirror = true, want false")
	}

	if fake.lastReq.Service != ServiceGitHub {
		t.Errorf("Service = %q, want %q", fake.lastReq.Service, ServiceGitHub)
	}
	if fake.lastReq.RepoName != "widgets" {
		t.Errorf("RepoName = %q, want widgets (derived from source URL)", fake.lastReq.RepoName)
	}
	if !fake.lastReq.LFS {
		t.Error("LFS = false, want true (IMPT-001 always migrates LFS objects)")
	}
	if fake.lastReq.AuthToken != "source-token" {
		t.Errorf("AuthToken = %q, want source-token", fake.lastReq.AuthToken)
	}
	if fake.lastReq.Issues || fake.lastReq.PullRequests || fake.lastReq.Wiki || fake.lastReq.Releases {
		t.Errorf("issue/PR/wiki/release history requested, want all false: %+v", fake.lastReq)
	}
	if fake.lastReq.Mirror {
		t.Error("Mirror requested on the wire, want false")
	}

	if !job.Done() {
		t.Fatal("job not terminal after a successful Run")
	}
	evs := job.Events()
	if evs[len(evs)-1].State != "succeeded" {
		t.Errorf("last event state = %q, want succeeded", evs[len(evs)-1].State)
	}
}

func TestRunEnablesMirrorSync(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token"}
	server := newTestServer(t, fake)

	opts := baseOptions(server, "target-token")
	opts.Mirror = true
	opts.MirrorInterval = 30 * time.Minute

	job := events.NewJob()
	result, err := Run(context.Background(), job, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !result.Mirror {
		t.Error("Result.Mirror = false, want true")
	}
	if !fake.lastReq.Mirror {
		t.Error("Mirror not requested on the wire")
	}
	if fake.lastReq.MirrorInterval != (30 * time.Minute).String() {
		t.Errorf("MirrorInterval = %q, want %q", fake.lastReq.MirrorInterval, (30 * time.Minute).String())
	}
}

func TestRunMirrorFalseOmitsInterval(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token"}
	server := newTestServer(t, fake)

	opts := baseOptions(server, "target-token")
	opts.MirrorInterval = 30 * time.Minute // set but Mirror is false

	job := events.NewJob()
	if _, err := Run(context.Background(), job, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.lastReq.MirrorInterval != "" {
		t.Errorf("MirrorInterval = %q, want empty when Mirror is false", fake.lastReq.MirrorInterval)
	}
}

func TestRunAutodetectsGitLab(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token"}
	server := newTestServer(t, fake)

	opts := baseOptions(server, "target-token")
	opts.SourceURL = "https://gitlab.com/acme/widgets"

	job := events.NewJob()
	if _, err := Run(context.Background(), job, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.lastReq.Service != ServiceGitLab {
		t.Errorf("Service = %q, want %q", fake.lastReq.Service, ServiceGitLab)
	}
}

func TestRunUnknownHostRequiresExplicitService(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token"}
	server := newTestServer(t, fake)

	opts := baseOptions(server, "target-token")
	opts.SourceURL = "https://git.internal.example.com/acme/widgets"

	job := events.NewJob()
	_, err := Run(context.Background(), job, opts)
	if err == nil {
		t.Fatal("Run with an undetectable host: want error, got nil")
	}
	if !job.Done() {
		t.Fatal("job not terminal after a validation failure")
	}
}

func TestRunExplicitServiceOverridesAutodetect(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token"}
	server := newTestServer(t, fake)

	opts := baseOptions(server, "target-token")
	opts.SourceURL = "https://git.internal.example.com/acme/widgets"
	opts.Service = "gitlab"

	job := events.NewJob()
	if _, err := Run(context.Background(), job, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.lastReq.Service != ServiceGitLab {
		t.Errorf("Service = %q, want %q", fake.lastReq.Service, ServiceGitLab)
	}
}

func TestRunRejectsUnsupportedService(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token"}
	server := newTestServer(t, fake)

	opts := baseOptions(server, "target-token")
	opts.Service = "bitbucket"

	job := events.NewJob()
	if _, err := Run(context.Background(), job, opts); err == nil {
		t.Fatal("Run with unsupported service: want error, got nil")
	}
}

func TestRunMissingRequiredFields(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token"}
	server := newTestServer(t, fake)
	base := baseOptions(server, "target-token")

	cases := []struct {
		name   string
		modify func(*Options)
	}{
		{"no target URL", func(o *Options) { o.TargetBaseURL = "" }},
		{"no target token", func(o *Options) { o.TargetToken = keystore.Secret{} }},
		{"no source URL", func(o *Options) { o.SourceURL = "" }},
		{"no source token", func(o *Options) { o.SourceToken = keystore.Secret{} }},
		{"no repo owner", func(o *Options) { o.RepoOwner = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := base
			tc.modify(&opts)
			job := events.NewJob()
			if _, err := Run(context.Background(), job, opts); err == nil {
				t.Fatalf("Run with %s: want error, got nil", tc.name)
			}
			if !job.Done() {
				t.Fatal("job not terminal after a validation failure")
			}
		})
	}
}

func TestRunTargetAPIErrorPropagates(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token", failStatus: http.StatusUnprocessableEntity, failMessage: "repository already exists"}
	server := newTestServer(t, fake)

	job := events.NewJob()
	_, err := Run(context.Background(), job, baseOptions(server, "target-token"))
	if err == nil || !strings.Contains(err.Error(), "repository already exists") {
		t.Fatalf("Run error = %v, want it to mention %q", err, "repository already exists")
	}
	if !job.Done() {
		t.Fatal("job not terminal after a failed migration")
	}
	evs := job.Events()
	if evs[len(evs)-1].State != "failed" {
		t.Errorf("last event state = %q, want failed", evs[len(evs)-1].State)
	}
}

func TestRunInvalidTargetTokenErrors(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token"}
	server := newTestServer(t, fake)

	job := events.NewJob()
	_, err := Run(context.Background(), job, baseOptions(server, "wrong-token"))
	if err == nil {
		t.Fatal("Run with wrong target token: want error, got nil")
	}
}

func TestRunDerivesRepoNameFromSourceURL(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token"}
	server := newTestServer(t, fake)

	opts := baseOptions(server, "target-token")
	opts.SourceURL = "https://github.com/acme/widgets.git"

	job := events.NewJob()
	if _, err := Run(context.Background(), job, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.lastReq.RepoName != "widgets" {
		t.Errorf("RepoName = %q, want widgets (stripped .git suffix)", fake.lastReq.RepoName)
	}
}

func TestRunExplicitRepoNameOverridesDerivation(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token"}
	server := newTestServer(t, fake)

	opts := baseOptions(server, "target-token")
	opts.RepoName = "renamed"

	job := events.NewJob()
	if _, err := Run(context.Background(), job, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.lastReq.RepoName != "renamed" {
		t.Errorf("RepoName = %q, want renamed", fake.lastReq.RepoName)
	}
}
