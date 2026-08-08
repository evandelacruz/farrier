package importrepo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

const wantAdminToken = "forge-admin-token"
const wantSourceToken = "source-token"

// fakeForge is a minimal stand-in for Forgejo's POST /api/v1/repos/migrate:
// enough to assert on the request Run sends and to exercise both a
// successful migration and a rejected one.
type fakeForge struct {
	gotRequest migrateRequest
	gotAuth    string

	failStatus  int
	failMessage string
}

func (f *fakeForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != migratePath || r.Method != http.MethodPost {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.gotAuth = r.Header.Get("Authorization")
	json.NewDecoder(r.Body).Decode(&f.gotRequest)

	if f.failStatus != 0 {
		w.WriteHeader(f.failStatus)
		json.NewEncoder(w).Encode(apiError{Message: f.failMessage})
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(migrateResponse{
		FullName:      f.gotRequest.RepoOwner + "/" + f.gotRequest.RepoName,
		CloneURL:      "https://forge.example/" + f.gotRequest.RepoOwner + "/" + f.gotRequest.RepoName + ".git",
		DefaultBranch: "main",
	})
}

func baseOptions(server *httptest.Server) Options {
	return Options{
		API:         server.URL,
		AdminToken:  keystore.NewSecret(wantAdminToken),
		SourceURL:   "https://github.com/acme/widgets.git",
		SourceToken: keystore.NewSecret(wantSourceToken),
		Owner:       "acme",
		Private:     true,
		HTTPClient:  server.Client(),
	}
}

func TestRunMigratesRepository(t *testing.T) {
	fake := &fakeForge{}
	server := httptest.NewServer(fake)
	defer server.Close()

	job := events.NewJob()
	result, err := Run(context.Background(), job, baseOptions(server))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.FullName != "acme/widgets" {
		t.Errorf("FullName = %q, want %q", result.FullName, "acme/widgets")
	}
	if result.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q, want %q", result.DefaultBranch, "main")
	}

	if fake.gotAuth != "token "+wantAdminToken {
		t.Errorf("Authorization header = %q, want %q", fake.gotAuth, "token "+wantAdminToken)
	}
	want := migrateRequest{
		CloneAddr: "https://github.com/acme/widgets.git",
		Service:   ServiceGitHub,
		AuthToken: wantSourceToken,
		RepoOwner: "acme",
		RepoName:  "widgets",
		Mirror:    false,
		LFS:       true,
		Private:   true,
	}
	if fake.gotRequest != want {
		t.Errorf("migrate request = %+v, want %+v", fake.gotRequest, want)
	}

	evs := job.Events()
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3 (step started, step succeeded, job succeeded): %+v", len(evs), evs)
	}
	if evs[0].Step != StepMigrate || evs[0].State != events.StateStarted {
		t.Errorf("evs[0] = %+v, want a StepMigrate started event", evs[0])
	}
	if evs[1].Step != StepMigrate || evs[1].State != events.StateSucceeded {
		t.Errorf("evs[1] = %+v, want a StepMigrate succeeded event", evs[1])
	}
	if evs[2].Step != "" || evs[2].State != events.StateSucceeded {
		t.Errorf("evs[2] = %+v, want the job-terminal succeeded event", evs[2])
	}
	if !job.Done() {
		t.Error("job.Done() = false, want true after Run succeeds")
	}
}

func TestRunDetectsGitLabService(t *testing.T) {
	fake := &fakeForge{}
	server := httptest.NewServer(fake)
	defer server.Close()

	opts := baseOptions(server)
	opts.SourceURL = "https://gitlab.com/acme/widgets.git"

	if _, err := Run(context.Background(), events.NewJob(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.gotRequest.Service != ServiceGitLab {
		t.Errorf("Service = %q, want %q", fake.gotRequest.Service, ServiceGitLab)
	}
}

func TestRunDerivesRepoNameFromSSHStyleURL(t *testing.T) {
	fake := &fakeForge{}
	server := httptest.NewServer(fake)
	defer server.Close()

	opts := baseOptions(server)
	opts.SourceURL = "git@github.com:acme/widgets.git"

	if _, err := Run(context.Background(), events.NewJob(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.gotRequest.RepoName != "widgets" {
		t.Errorf("RepoName = %q, want %q", fake.gotRequest.RepoName, "widgets")
	}
}

func TestRunLeavesOwnerEmptyForForgeDefault(t *testing.T) {
	fake := &fakeForge{}
	server := httptest.NewServer(fake)
	defer server.Close()

	opts := baseOptions(server)
	opts.Owner = ""

	if _, err := Run(context.Background(), events.NewJob(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.gotRequest.RepoOwner != "" {
		t.Errorf("RepoOwner = %q, want empty so the forge defaults it", fake.gotRequest.RepoOwner)
	}
}

func TestRunFailsWhenServiceCannotBeInferred(t *testing.T) {
	fake := &fakeForge{}
	server := httptest.NewServer(fake)
	defer server.Close()

	opts := baseOptions(server)
	opts.SourceURL = "https://git.example.internal/acme/widgets.git"

	job := events.NewJob()
	if _, err := Run(context.Background(), job, opts); err == nil {
		t.Fatal("Run: want error for an unrecognized source host, got nil")
	}
	assertJobFailed(t, job)
}

func TestRunPropagatesMigrateAPIError(t *testing.T) {
	fake := &fakeForge{failStatus: http.StatusUnprocessableEntity, failMessage: "repository already exists"}
	server := httptest.NewServer(fake)
	defer server.Close()

	job := events.NewJob()
	_, err := Run(context.Background(), job, baseOptions(server))
	if err == nil {
		t.Fatal("Run: want error when the forge rejects the migration, got nil")
	}
	if !strings.Contains(err.Error(), "repository already exists") {
		t.Errorf("error = %v, want it to name the forge's reason", err)
	}
	assertJobFailed(t, job)
}

func assertJobFailed(t *testing.T, job *events.Job) {
	t.Helper()
	evs := job.Events()
	if len(evs) == 0 {
		t.Fatal("job recorded no events")
	}
	last := evs[len(evs)-1]
	if last.Step != "" || last.State != events.StateFailed {
		t.Errorf("last event = %+v, want the job-terminal failed event", last)
	}
	if !job.Done() {
		t.Error("job.Done() = false, want true after Run fails")
	}
}

func TestOptionsValidate(t *testing.T) {
	valid := Options{
		API:         "https://forge.example",
		AdminToken:  keystore.NewSecret("t"),
		SourceURL:   "https://github.com/acme/widgets.git",
		SourceToken: keystore.NewSecret("s"),
	}

	cases := []struct {
		name string
		opts Options
	}{
		{"missing API", func() Options { o := valid; o.API = ""; return o }()},
		{"missing admin token", func() Options { o := valid; o.AdminToken = keystore.Secret{}; return o }()},
		{"missing source URL", func() Options { o := valid; o.SourceURL = ""; return o }()},
		{"missing source token", func() Options { o := valid; o.SourceToken = keystore.Secret{}; return o }()},
		{"invalid service", func() Options { o := valid; o.Service = "bitbucket"; return o }()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.opts.validate(); err == nil {
				t.Fatalf("validate(): want error for %s, got nil", c.name)
			}
		})
	}

	if err := valid.validate(); err != nil {
		t.Fatalf("validate(): want nil for a fully populated Options, got %v", err)
	}
}

func TestRepoNameFromURL(t *testing.T) {
	cases := []struct {
		url, want string
		wantErr   bool
	}{
		{"https://github.com/acme/widgets.git", "widgets", false},
		{"https://github.com/acme/widgets", "widgets", false},
		{"https://github.com/acme/widgets/", "widgets", false},
		{"git@github.com:acme/widgets.git", "widgets", false},
		{"widgets", "", true},
	}
	for _, c := range cases {
		got, err := repoNameFromURL(c.url)
		if c.wantErr {
			if err == nil {
				t.Errorf("repoNameFromURL(%q): want error, got %q", c.url, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("repoNameFromURL(%q): unexpected error: %v", c.url, err)
		}
		if got != c.want {
			t.Errorf("repoNameFromURL(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}
