// Package importer implements `import` (IMPT-001, IMPT-002, IMPT-003):
// bringing repositories in from GitHub or GitLab onto a Farrier instance.
//
// Per spec.md "Importing repositories", import wraps Forgejo's built-in
// migration API rather than reimplementing git transport: code, full
// history, LFS objects, and the default branch travel in one call to
// Forgejo's own `POST /api/v1/repos/migrate`, and continuous mirror sync
// (IMPT-002) is the same call with its mirror fields set. Issue and
// pull-request history is deliberately left on the source forge — that
// call sets issues/pull_requests/wiki/releases to false unconditionally,
// matching the settled design rather than exposing it as a choice.
//
// Run migrates one repository; RunBatch migrates many against one target
// in a single job, reporting each repository's own success or failure
// (IMPT-003). Either way, a migration that fails leaves no
// partially-registered repository behind: migrate's failure paths attempt
// to delete whatever Forgejo may have registered on the target before the
// failure.
package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// StepMigrate identifies the migration step in import's job event stream.
const StepMigrate = "migrate"

// ServiceGitHub and ServiceGitLab are the only source services IMPT-001
// commits to. Service, below, autodetects one of these two from the
// source URL's host when not given explicitly.
const (
	ServiceGitHub = "github"
	ServiceGitLab = "gitlab"
)

// Options configures one repository import.
type Options struct {
	// TargetBaseURL is the Farrier instance's base URL, e.g.
	// "https://git.example.com". Required.
	TargetBaseURL string
	// TargetToken authenticates to the target instance's API. Required.
	TargetToken keystore.Secret

	// SourceURL is the repository's clone URL on the source forge, e.g.
	// "https://github.com/owner/repo". Required.
	SourceURL string
	// SourceToken authenticates to the source forge for the clone and LFS
	// transfer. Required.
	SourceToken keystore.Secret
	// Service is "github" or "gitlab". Empty autodetects from SourceURL's
	// host; detection fails closed if the host doesn't obviously say
	// which, so callers of a self-hosted instance must set it explicitly.
	Service string

	// RepoOwner is the owner (user or organization) the repository lands
	// under on the target instance. Required.
	RepoOwner string
	// RepoName is the repository's name on the target instance. Empty
	// derives it from the last path segment of SourceURL.
	RepoName string
	// Private marks the imported repository private on the target
	// instance. Defaults to true (RepoOwner is the target's own team,
	// not the public) when Options is built through Run's CLI caller;
	// the zero value here is false, so callers must set it explicitly.
	Private bool

	// Mirror requests continuous mirror sync from the source (IMPT-002).
	Mirror bool
	// MirrorInterval is how often Forgejo re-pulls the source once Mirror
	// is set. Ignored when Mirror is false. Zero with Mirror true lets
	// Forgejo apply its own configured default.
	MirrorInterval time.Duration

	// HTTPClient overrides the client used for the target API call; nil
	// uses http.DefaultClient. Tests point this at an httptest.Server.
	HTTPClient *http.Client
}

// Result reports the repository Run created on the target instance.
type Result struct {
	ID       int64
	FullName string
	HTMLURL  string
	Mirror   bool
}

// Run migrates one repository from Options.SourceURL onto the target
// instance via Forgejo's migration API, reporting progress through job
// (CORE-002) and owning the job's terminal event — job.Succeeded or
// job.Failed — the way deploy.Up owns it for `up`.
func Run(ctx context.Context, job *events.Job, opts Options) (Result, error) {
	result, err := runOne(ctx, job, opts, StepMigrate)
	if err != nil {
		job.Failed(err.Error())
		return Result{}, err
	}
	job.Succeeded(successDetail(opts, result))
	return result, nil
}

// runOne migrates one repository and emits its Started/Succeeded|Failed
// events scoped to step, but never a job-terminal (empty-step) event —
// Run and RunBatch each own the job's single terminal event and call this
// once per repository they migrate.
func runOne(ctx context.Context, job *events.Job, opts Options, step string) (Result, error) {
	service, repoName, err := resolve(opts)
	if err != nil {
		job.Emit(step, events.StateFailed, err.Error())
		return Result{}, fmt.Errorf("importer: %w", err)
	}

	job.Started(step, fmt.Sprintf(
		"migrating %s from %s to %s/%s (mirror=%t)",
		opts.SourceURL, service, opts.RepoOwner, repoName, opts.Mirror,
	))

	result, err := migrate(ctx, opts, service, repoName)
	if err != nil {
		detail := fmt.Sprintf("migrate %s: %s", opts.SourceURL, err)
		job.Emit(step, events.StateFailed, detail)
		return Result{}, fmt.Errorf("importer: migrate %s: %w", opts.SourceURL, err)
	}

	job.Emit(step, events.StateSucceeded, successDetail(opts, result))
	return result, nil
}

// successDetail is the detail string for one repository's successful
// migration, shared by Run's job-terminal event and runOne's per-step
// event.
func successDetail(opts Options, result Result) string {
	detail := fmt.Sprintf("imported %s as %s", opts.SourceURL, result.FullName)
	if opts.Mirror {
		detail += " (mirror sync enabled)"
	}
	return detail
}

// resolve validates opts and fills in Service/RepoName defaults, without
// making any network call.
func resolve(opts Options) (service, repoName string, err error) {
	if strings.TrimSpace(opts.TargetBaseURL) == "" {
		return "", "", fmt.Errorf("target base URL is required")
	}
	if opts.TargetToken.Reveal() == "" {
		return "", "", fmt.Errorf("target token is required")
	}
	if strings.TrimSpace(opts.SourceURL) == "" {
		return "", "", fmt.Errorf("source URL is required")
	}
	if opts.SourceToken.Reveal() == "" {
		return "", "", fmt.Errorf("source token is required")
	}
	if strings.TrimSpace(opts.RepoOwner) == "" {
		return "", "", fmt.Errorf("repo owner is required")
	}

	parsed, err := url.Parse(opts.SourceURL)
	if err != nil {
		return "", "", fmt.Errorf("parse source URL: %w", err)
	}

	service = strings.ToLower(strings.TrimSpace(opts.Service))
	if service == "" {
		service, err = detectService(parsed.Hostname())
		if err != nil {
			return "", "", err
		}
	} else if service != ServiceGitHub && service != ServiceGitLab {
		return "", "", fmt.Errorf("unsupported service %q: must be %q or %q", opts.Service, ServiceGitHub, ServiceGitLab)
	}

	repoName = strings.TrimSpace(opts.RepoName)
	if repoName == "" {
		repoName = strings.TrimSuffix(lastPathSegment(parsed), ".git")
		if repoName == "" {
			return "", "", fmt.Errorf("repo name is required: could not derive one from source URL %q", opts.SourceURL)
		}
	}

	return service, repoName, nil
}

// lastPathSegment returns the last non-empty segment of parsed's URL path.
func lastPathSegment(parsed *url.URL) string {
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) == 0 {
		return ""
	}
	return segments[len(segments)-1]
}

// detectService infers github or gitlab from host, failing closed rather
// than guessing when the host doesn't obviously name one — a self-hosted
// GitLab instance needs Options.Service set explicitly.
func detectService(host string) (string, error) {
	host = strings.ToLower(host)
	switch {
	case host == "github.com" || strings.HasSuffix(host, ".github.com"):
		return ServiceGitHub, nil
	case host == "gitlab.com" || strings.HasSuffix(host, ".gitlab.com"):
		return ServiceGitLab, nil
	default:
		return "", fmt.Errorf("could not detect source service from host %q: set Service explicitly to %q or %q", host, ServiceGitHub, ServiceGitLab)
	}
}

// migrateRequest is Forgejo's MigrateRepoOptions (shared with upstream
// Gitea): the subset of fields import needs to set. Issue/PR/wiki/release
// history is left on the source forge unconditionally (spec.md), so those
// fields are never exposed as Options — they are always false here.
type migrateRequest struct {
	CloneAddr      string `json:"clone_addr"`
	AuthToken      string `json:"auth_token,omitempty"`
	Service        string `json:"service"`
	RepoOwner      string `json:"repo_owner"`
	RepoName       string `json:"repo_name"`
	Mirror         bool   `json:"mirror"`
	MirrorInterval string `json:"mirror_interval,omitempty"`
	LFS            bool   `json:"lfs"`
	Private        bool   `json:"private"`
	Wiki           bool   `json:"wiki"`
	Issues         bool   `json:"issues"`
	PullRequests   bool   `json:"pull_requests"`
	Releases       bool   `json:"releases"`
}

// migrateResponse is the subset of Forgejo's Repository object Run reports
// back as a Result.
type migrateResponse struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
}

// apiError is Forgejo's standard error envelope.
type apiError struct {
	Message string `json:"message"`
}

func migrate(ctx context.Context, opts Options, service, repoName string) (Result, error) {
	req := migrateRequest{
		CloneAddr: opts.SourceURL,
		AuthToken: opts.SourceToken.Reveal(),
		Service:   service,
		RepoOwner: opts.RepoOwner,
		RepoName:  repoName,
		Mirror:    opts.Mirror,
		LFS:       true,
		Private:   opts.Private,
	}
	if opts.Mirror && opts.MirrorInterval > 0 {
		req.MirrorInterval = opts.MirrorInterval.String()
	}

	body, err := json.Marshal(req)
	if err != nil {
		return Result{}, fmt.Errorf("encode request: %w", err)
	}

	endpoint := strings.TrimRight(opts.TargetBaseURL, "/") + "/api/v1/repos/migrate"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "token "+opts.TargetToken.Reveal())

	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return Result{}, withCleanup(opts, repoName, fmt.Errorf("call target API: %w", err))
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{}, withCleanup(opts, repoName, fmt.Errorf("read response: %w", err))
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr apiError
		if err := json.Unmarshal(respBody, &apiErr); err == nil && apiErr.Message != "" {
			return Result{}, withCleanup(opts, repoName, fmt.Errorf("target API returned %d: %s", resp.StatusCode, apiErr.Message))
		}
		return Result{}, withCleanup(opts, repoName, fmt.Errorf("target API returned %d: %s", resp.StatusCode, trimBody(respBody)))
	}

	// A 2xx response means Forgejo genuinely created the repository — a
	// decode failure past this point is a client-side parsing bug, not a
	// partial registration, so no cleanup: deleting a successfully
	// created repository here would destroy real, complete work.
	var decoded migrateResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return Result{}, fmt.Errorf("decode response: %w", err)
	}

	return Result{
		ID:       decoded.ID,
		FullName: decoded.FullName,
		HTMLURL:  decoded.HTMLURL,
		Mirror:   opts.Mirror,
	}, nil
}

func trimBody(body []byte) string {
	const max = 300
	s := strings.TrimSpace(string(body))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// withCleanup attempts to delete any repository Forgejo may have
// registered on the target before migrateErr occurred — IMPT-003's "no
// partially-registered repository on failure". It runs best-effort: a
// cleanup failure is reported alongside, never in place of, the original
// migration error, and a 404 (nothing was ever registered) is not an
// error at all.
func withCleanup(opts Options, repoName string, migrateErr error) error {
	if cleanupErr := deleteRepo(opts, repoName); cleanupErr != nil {
		return fmt.Errorf("%w (cleanup also failed, %s/%s may need manual removal from the target: %s)", migrateErr, opts.RepoOwner, repoName, cleanupErr)
	}
	return migrateErr
}

// deleteRepo removes owner/repoName from the target instance. It runs on
// its own detached, timed-out context rather than the caller's — the
// failure that triggered cleanup may itself be that context expiring or
// being canceled, and cleanup must still get a chance to run.
func deleteRepo(opts Options, repoName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	endpoint := strings.TrimRight(opts.TargetBaseURL, "/") + "/api/v1/repos/" + url.PathEscape(opts.RepoOwner) + "/" + url.PathEscape(repoName)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build cleanup request: %w", err)
	}
	req.Header.Set("Authorization", "token "+opts.TargetToken.Reveal())

	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("call target API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("target API returned %d: %s", resp.StatusCode, trimBody(body))
	}
	return nil
}
