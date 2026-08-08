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
// and DELETE /api/v1/repos/{owner}/{repo} endpoints: enough to exercise
// Run's request shape, response handling, and IMPT-003 cleanup-on-failure.
type fakeForgejo struct {
	wantToken    string
	failStatus   int
	failMessage  string
	lastReq      migrateRequest
	nextID       int64
	deleteStatus int // 0 defaults to 404 (nothing registered)
	deletedRepos []string
}

func (f *fakeForgejo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/repos/") {
		if got := r.Header.Get("Authorization"); got != "token "+f.wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.deletedRepos = append(f.deletedRepos, strings.TrimPrefix(r.URL.Path, "/api/v1/repos/"))
		status := f.deleteStatus
		if status == 0 {
			status = http.StatusNotFound
		}
		w.WriteHeader(status)
		return
	}

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

// IMPT-003: a failed migration must leave no partially-registered
// repository behind.

func TestRunFailureTriggersCleanup(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token", failStatus: http.StatusInternalServerError, failMessage: "clone interrupted"}
	server := newTestServer(t, fake)

	job := events.NewJob()
	_, err := Run(context.Background(), job, baseOptions(server, "target-token"))
	if err == nil {
		t.Fatal("Run with a failing migration: want error, got nil")
	}
	if len(fake.deletedRepos) != 1 || fake.deletedRepos[0] != "acme/widgets" {
		t.Errorf("deletedRepos = %v, want [\"acme/widgets\"] (cleanup called after migration failure)", fake.deletedRepos)
	}
}

func TestRunCleanup404IsNotAnError(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token", failStatus: http.StatusUnprocessableEntity, failMessage: "repository already exists"}
	server := newTestServer(t, fake)

	job := events.NewJob()
	_, err := Run(context.Background(), job, baseOptions(server, "target-token"))
	if err == nil {
		t.Fatal("Run: want error, got nil")
	}
	if strings.Contains(err.Error(), "cleanup also failed") {
		t.Errorf("error = %v, want no cleanup-failed suffix when cleanup 404s (nothing to clean up)", err)
	}
}

func TestRunCleanupFailureIsReportedAlongsideOriginalError(t *testing.T) {
	fake := &fakeForgejo{
		wantToken:    "target-token",
		failStatus:   http.StatusInternalServerError,
		failMessage:  "clone interrupted",
		deleteStatus: http.StatusInternalServerError,
	}
	server := newTestServer(t, fake)

	job := events.NewJob()
	_, err := Run(context.Background(), job, baseOptions(server, "target-token"))
	if err == nil {
		t.Fatal("Run: want error, got nil")
	}
	if !strings.Contains(err.Error(), "clone interrupted") {
		t.Errorf("error = %v, want it to still mention the original migration failure", err)
	}
	if !strings.Contains(err.Error(), "cleanup also failed") {
		t.Errorf("error = %v, want it to mention the cleanup failure too", err)
	}
}

func TestRunSuccessNeverTriggersCleanup(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token"}
	server := newTestServer(t, fake)

	job := events.NewJob()
	if _, err := Run(context.Background(), job, baseOptions(server, "target-token")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.deletedRepos) != 0 {
		t.Errorf("deletedRepos = %v, want none after a successful migration", fake.deletedRepos)
	}
}

// RunBatch (IMPT-003: per-repository batch reporting).

func TestRunBatchReportsEachRepository(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token"}
	server := newTestServer(t, fake)

	base := baseOptions(server, "target-token")
	widgets := base
	widgets.SourceURL = "https://github.com/acme/widgets"
	gadgets := base
	gadgets.SourceURL = "https://github.com/acme/gadgets"

	job := events.NewJob()
	result, err := RunBatch(context.Background(), job, BatchOptions{Repos: []Options{widgets, gadgets}})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if len(result.Repos) != 2 {
		t.Fatalf("len(result.Repos) = %d, want 2", len(result.Repos))
	}
	if result.Repos[0].Err != nil || result.Repos[0].Result.FullName != "acme/widgets" {
		t.Errorf("result.Repos[0] = %+v, want a successful widgets import", result.Repos[0])
	}
	if result.Repos[1].Err != nil || result.Repos[1].Result.FullName != "acme/gadgets" {
		t.Errorf("result.Repos[1] = %+v, want a successful gadgets import", result.Repos[1])
	}
	if result.Failures() != 0 {
		t.Errorf("Failures() = %d, want 0", result.Failures())
	}
	if !job.Done() {
		t.Fatal("job not terminal after RunBatch")
	}
	evs := job.Events()
	if evs[len(evs)-1].State != "succeeded" {
		t.Errorf("last event state = %q, want succeeded", evs[len(evs)-1].State)
	}
}

func TestRunBatchContinuesPastOneFailureAndReportsBoth(t *testing.T) {
	fake := &fakeForgejo{wantToken: "target-token"}
	server := newTestServer(t, fake)

	base := baseOptions(server, "target-token")
	ok := base
	ok.SourceURL = "https://github.com/acme/widgets"
	bad := base
	bad.SourceURL = "https://github.com/acme/gadgets"
	bad.RepoOwner = "" // invalid: fails validation before any network call

	job := events.NewJob()
	result, err := RunBatch(context.Background(), job, BatchOptions{Repos: []Options{bad, ok}})
	if err == nil {
		t.Fatal("RunBatch with one invalid repository: want error, got nil")
	}
	if result.Failures() != 1 {
		t.Errorf("Failures() = %d, want 1", result.Failures())
	}
	if result.Repos[0].Err == nil {
		t.Error("result.Repos[0].Err = nil, want the validation error")
	}
	if result.Repos[1].Err != nil {
		t.Errorf("result.Repos[1].Err = %v, want nil (the valid repository still imports despite the other's failure)", result.Repos[1].Err)
	}
	if !job.Done() {
		t.Fatal("job not terminal after RunBatch")
	}
	evs := job.Events()
	if evs[len(evs)-1].State != "failed" {
		t.Errorf("last event state = %q, want failed", evs[len(evs)-1].State)
	}
}

func TestRunBatchRejectsEmptyRepoList(t *testing.T) {
	job := events.NewJob()
	_, err := RunBatch(context.Background(), job, BatchOptions{})
	if err == nil {
		t.Fatal("RunBatch with no repositories: want error, got nil")
	}
	if !job.Done() {
		t.Fatal("job not terminal after an empty batch")
	}
}

// Manifest (batch import's YAML repository list).

func TestParseManifestReadsRepos(t *testing.T) {
	data := []byte(`
repos:
  - source: https://github.com/acme/widgets
    owner: acme
  - source: https://github.com/acme/gadgets
    owner: other
    name: renamed-gadgets
    service: github
    private: false
    mirror: true
    mirrorInterval: 30m
`)
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if len(m.Repos) != 2 {
		t.Fatalf("len(m.Repos) = %d, want 2", len(m.Repos))
	}
	if m.Repos[1].Name != "renamed-gadgets" || !m.Repos[1].Mirror || m.Repos[1].MirrorInterval != "30m" {
		t.Errorf("m.Repos[1] = %+v, want the full override set", m.Repos[1])
	}
	if m.Repos[1].Private == nil || *m.Repos[1].Private {
		t.Errorf("m.Repos[1].Private = %v, want false", m.Repos[1].Private)
	}
}

func TestParseManifestRejectsEmpty(t *testing.T) {
	if _, err := ParseManifest([]byte(`repos: []`)); err == nil {
		t.Fatal("ParseManifest with no repos: want error, got nil")
	}
}

func TestParseManifestRejectsMissingSource(t *testing.T) {
	if _, err := ParseManifest([]byte("repos:\n  - owner: acme\n")); err == nil {
		t.Fatal("ParseManifest with no source: want error, got nil")
	}
}

func TestManifestRepoOptionsLayersOverDefaults(t *testing.T) {
	m, err := ParseManifest([]byte(`
repos:
  - source: https://github.com/acme/widgets
  - source: https://github.com/acme/gadgets
    owner: other-org
    private: false
`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	defaults := Options{
		TargetBaseURL: "https://target.example.com",
		TargetToken:   keystore.NewSecret("target-token"),
		SourceToken:   keystore.NewSecret("source-token"),
		RepoOwner:     "acme",
		Private:       true,
	}

	opts, err := m.RepoOptions(defaults)
	if err != nil {
		t.Fatalf("RepoOptions: %v", err)
	}
	if len(opts) != 2 {
		t.Fatalf("len(opts) = %d, want 2", len(opts))
	}
	if opts[0].RepoOwner != "acme" || !opts[0].Private {
		t.Errorf("opts[0] = %+v, want the batch default owner/private", opts[0])
	}
	if opts[1].RepoOwner != "other-org" || opts[1].Private {
		t.Errorf("opts[1] = %+v, want its own owner/private overrides", opts[1])
	}
}

func TestManifestRepoOptionsRequiresOwner(t *testing.T) {
	m, err := ParseManifest([]byte(`repos:
  - source: https://github.com/acme/widgets
`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if _, err := m.RepoOptions(Options{TargetBaseURL: "x", TargetToken: keystore.NewSecret("t"), SourceToken: keystore.NewSecret("s")}); err == nil {
		t.Fatal("RepoOptions with no owner anywhere: want error, got nil")
	}
}
