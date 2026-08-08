// Package importrepo implements IMPT-001: bringing an existing repository
// in from GitHub or GitLab, given the source repository's URL and a token,
// by wrapping Forgejo's own repository migration API (spec.md "Importing
// repositories") rather than reimplementing git/LFS transfer.
//
// Run calls POST /api/v1/repos/migrate against the target forge (the wire
// format is Forgejo/Gitea's stable structs.MigrateRepoOptions), asking for
// the full git history and LFS objects and explicitly excluding issues,
// pull requests, wiki, milestones, labels, and releases — those stay on
// the source forge (spec.md "Importing repositories"). Continuous mirror
// sync (IMPT-002) and multi-repository batch reporting (IMPT-003) are
// later additions to the same command.
package importrepo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// StepMigrate identifies the (only) step Run emits through the job's event
// stream (CORE-002).
const StepMigrate = "migrate"

// Service names the source forge a repository is migrated from. These are
// the two values IMPT-001 requires; Forgejo's migrate API accepts others
// (gitea, gogs, onedev, plain git) that Run does not expose.
const (
	ServiceGitHub = "github"
	ServiceGitLab = "gitlab"
)

// migratePath is the Forgejo API endpoint Run calls, relative to Options.API.
const migratePath = "/api/v1/repos/migrate"

// Options configures one repository migration.
type Options struct {
	// API is the destination forge's base URL, e.g. "https://git.example.com"
	// (the bundle's own domain once `up` has deployed it — UP-002).
	API string
	// AdminToken authenticates against the destination forge's API. It
	// must belong to an account with rights to create repositories under
	// Owner (or, if Owner is empty, under itself).
	AdminToken keystore.Secret

	// SourceURL is the source repository's clone address, e.g.
	// "https://github.com/owner/repo.git".
	SourceURL string
	// SourceToken authenticates against the source forge so Forgejo's
	// migration can read a private repository's code, history, and LFS
	// objects.
	SourceToken keystore.Secret
	// Service names the source forge: ServiceGitHub or ServiceGitLab. If
	// empty, Run infers it from SourceURL's host.
	Service string

	// Owner is the Forgejo user or organization the migrated repository is
	// created under. Empty lets the destination forge default it to
	// AdminToken's own account.
	Owner string
	// RepoName is the migrated repository's name on Forgejo. Empty derives
	// it from the last path segment of SourceURL.
	RepoName string
	// Private marks the migrated repository private.
	Private bool

	// HTTPClient makes the migrate request; nil uses http.DefaultClient.
	// Tests point this at an httptest.Server's client.
	HTTPClient *http.Client
}

// Result is what Forgejo's migrate API reports back about the repository it
// created.
type Result struct {
	// FullName is "owner/name" on the destination forge.
	FullName string
	// CloneURL is the migrated repository's clone address on the
	// destination forge.
	CloneURL string
	// DefaultBranch is the default branch Forgejo carried over from the
	// source repository.
	DefaultBranch string
}

// Run migrates one repository per opts (IMPT-001): it emits a StepMigrate
// started event, calls Forgejo's migrate API, and emits the job's terminal
// event — Succeeded naming the migrated repository and its default branch,
// or Failed naming the reason. It owns job's terminal event: it calls
// job.Succeeded or job.Failed exactly once and returns the same error it
// reported through job.
func Run(ctx context.Context, job *events.Job, opts Options) (*Result, error) {
	result, err := run(ctx, job, opts)
	if err != nil {
		job.Failed(err.Error())
		return nil, err
	}
	job.Succeeded(fmt.Sprintf("migrated %s (default branch %q)", result.FullName, result.DefaultBranch))
	return result, nil
}

func run(ctx context.Context, job *events.Job, opts Options) (*Result, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	service := opts.Service
	if service == "" {
		var err error
		service, err = detectService(opts.SourceURL)
		if err != nil {
			return nil, fmt.Errorf("importrepo: %w", err)
		}
	}

	repoName := opts.RepoName
	if repoName == "" {
		var err error
		repoName, err = repoNameFromURL(opts.SourceURL)
		if err != nil {
			return nil, fmt.Errorf("importrepo: %w", err)
		}
	}

	job.Started(StepMigrate, fmt.Sprintf("migrating %s into %s", opts.SourceURL, destination(opts.Owner, repoName)))

	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	result, err := doMigrate(ctx, client, opts, service, repoName)
	if err != nil {
		job.Emit(StepMigrate, events.StateFailed, err.Error())
		return nil, fmt.Errorf("importrepo: migrate %s: %w", opts.SourceURL, err)
	}

	job.Emit(StepMigrate, events.StateSucceeded, fmt.Sprintf("migrated into %s", result.FullName))
	return result, nil
}

func (o Options) validate() error {
	if strings.TrimSpace(o.API) == "" {
		return fmt.Errorf("importrepo: destination forge API URL is required")
	}
	if o.AdminToken.Reveal() == "" {
		return fmt.Errorf("importrepo: destination forge admin token is required")
	}
	if strings.TrimSpace(o.SourceURL) == "" {
		return fmt.Errorf("importrepo: source URL is required")
	}
	if o.SourceToken.Reveal() == "" {
		return fmt.Errorf("importrepo: source token is required")
	}
	if o.Service != "" && o.Service != ServiceGitHub && o.Service != ServiceGitLab {
		return fmt.Errorf("importrepo: service must be %q or %q, got %q", ServiceGitHub, ServiceGitLab, o.Service)
	}
	return nil
}

// destination formats "owner/name" for a log line, falling back to the
// migrate API's own default-owner behavior when owner is empty.
func destination(owner, repoName string) string {
	if owner == "" {
		return repoName + " (default owner)"
	}
	return owner + "/" + repoName
}

// detectService infers the source service from sourceURL's host, since
// IMPT-001 only requires a source URL and token — not a separate service
// selector — for the two forges it targets.
func detectService(sourceURL string) (string, error) {
	lower := strings.ToLower(sourceURL)
	switch {
	case strings.Contains(lower, "github.com"):
		return ServiceGitHub, nil
	case strings.Contains(lower, "gitlab.com"):
		return ServiceGitLab, nil
	default:
		return "", fmt.Errorf("cannot infer source service (github or gitlab) from %q; set Service explicitly", sourceURL)
	}
}

// repoNameFromURL derives a repository name from the last path segment of a
// clone URL, over both URL and SCP-style ("git@host:owner/repo.git")
// addresses, stripping a trailing ".git".
func repoNameFromURL(sourceURL string) (string, error) {
	trimmed := strings.TrimSuffix(strings.TrimRight(sourceURL, "/"), ".git")
	idx := strings.LastIndexAny(trimmed, "/:")
	if idx == -1 || idx == len(trimmed)-1 {
		return "", fmt.Errorf("cannot derive a repository name from %q; set RepoName explicitly", sourceURL)
	}
	return trimmed[idx+1:], nil
}

// migrateRequest is Forgejo's structs.MigrateRepoOptions, restricted to the
// fields IMPT-001 needs. Issues, pull requests, wiki, milestones, labels,
// and releases are left at their zero value (false) so none of that
// migrates — spec.md "Importing repositories": that history stays on the
// source forge.
type migrateRequest struct {
	CloneAddr string `json:"clone_addr"`
	Service   string `json:"service"`
	AuthToken string `json:"auth_token"`
	RepoOwner string `json:"repo_owner,omitempty"`
	RepoName  string `json:"repo_name"`
	Mirror    bool   `json:"mirror"`
	LFS       bool   `json:"lfs"`
	Private   bool   `json:"private"`
}

// migrateResponse is the subset of Forgejo's Repository object Run reports
// back to the caller.
type migrateResponse struct {
	FullName      string `json:"full_name"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
}

// apiError is the error envelope Forgejo's API returns on a non-2xx
// response: {"message": "...", "url": "..."}.
type apiError struct {
	Message string `json:"message"`
}

// doMigrate calls Forgejo's migrate API and decodes its response.
func doMigrate(ctx context.Context, client *http.Client, opts Options, service, repoName string) (*Result, error) {
	body := migrateRequest{
		CloneAddr: opts.SourceURL,
		Service:   service,
		AuthToken: opts.SourceToken.Reveal(),
		RepoOwner: opts.Owner,
		RepoName:  repoName,
		Mirror:    false,
		LFS:       true,
		Private:   opts.Private,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode migrate request: %w", err)
	}

	url := strings.TrimRight(opts.API, "/") + migratePath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("build migrate request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+opts.AdminToken.Reveal())

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("migrate request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read migrate response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("forge returned %d: %s", resp.StatusCode, migrateErrorDetail(respBody))
	}

	var out migrateResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode migrate response: %w", err)
	}
	return &Result{FullName: out.FullName, CloneURL: out.CloneURL, DefaultBranch: out.DefaultBranch}, nil
}

func migrateErrorDetail(body []byte) string {
	var apiErr apiError
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Message != "" {
		return apiErr.Message
	}
	if s := strings.TrimSpace(string(body)); s != "" {
		return s
	}
	return "no error detail returned"
}
