package importer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/events"
	"gopkg.in/yaml.v3"
)

// RepoResult reports one repository's outcome within a batch import
// (IMPT-003): the source it came from, and either its Result or the error
// that failed it — never both.
type RepoResult struct {
	SourceURL string
	Result    Result
	Err       error
}

// BatchOptions configures a batch import: every repository in Repos
// migrates against the same target instance, in order. Each Options
// carries its own source, service, and target-side identity the way a
// single Run's Options does.
type BatchOptions struct {
	Repos []Options
}

// BatchResult reports IMPT-003's per-repository outcomes for a batch
// import, in the same order as BatchOptions.Repos.
type BatchResult struct {
	Repos []RepoResult
}

// Failures reports how many repositories in the batch failed.
func (r BatchResult) Failures() int {
	n := 0
	for _, repo := range r.Repos {
		if repo.Err != nil {
			n++
		}
	}
	return n
}

// RunBatch migrates every repository in opts.Repos against the target
// instance, continuing past a single repository's failure so the rest of
// the batch still runs, and reports each repository's outcome
// individually (IMPT-003) through the returned BatchResult. It owns the
// job's single terminal event: the job succeeds only if every repository
// in the batch succeeded, and fails otherwise — the per-repository detail
// lives in BatchResult and in each repository's own step events, not in
// the terminal event.
func RunBatch(ctx context.Context, job *events.Job, opts BatchOptions) (BatchResult, error) {
	if len(opts.Repos) == 0 {
		err := fmt.Errorf("importer: at least one repository is required")
		job.Failed(err.Error())
		return BatchResult{}, err
	}

	result := BatchResult{Repos: make([]RepoResult, 0, len(opts.Repos))}
	for i, repoOpts := range opts.Repos {
		step := fmt.Sprintf("%s:%d", StepMigrate, i+1)
		res, err := runOne(ctx, job, repoOpts, step)
		result.Repos = append(result.Repos, RepoResult{
			SourceURL: repoOpts.SourceURL,
			Result:    res,
			Err:       err,
		})
	}

	failures := result.Failures()
	detail := fmt.Sprintf("imported %d/%d repositories", len(opts.Repos)-failures, len(opts.Repos))
	if failures > 0 {
		job.Failed(detail)
		return result, fmt.Errorf("importer: %d of %d repositories failed", failures, len(opts.Repos))
	}
	job.Succeeded(detail)
	return result, nil
}

// ManifestEntry describes one repository within a batch import manifest:
// everything Run needs for that repository except the credentials and
// target instance shared by the whole batch. Per spec.md, key material is
// never written to a file, so a manifest never carries a token — Manifest
// entries layer over a defaults Options that already has TargetBaseURL,
// TargetToken, and SourceToken set.
//
// It also doubles as the wire shape for POST /import's batch "repos" field
// (internal/api), so the field tags cover both YAML (the -file manifest)
// and JSON (the API request body).
type ManifestEntry struct {
	Source         string `yaml:"source" json:"source"`
	Service        string `yaml:"service,omitempty" json:"service,omitempty"`
	Owner          string `yaml:"owner,omitempty" json:"owner,omitempty"`
	Name           string `yaml:"name,omitempty" json:"name,omitempty"`
	Private        *bool  `yaml:"private,omitempty" json:"private,omitempty"`
	Mirror         *bool  `yaml:"mirror,omitempty" json:"mirror,omitempty"`
	MirrorInterval string `yaml:"mirrorInterval,omitempty" json:"mirrorInterval,omitempty"`
}

// Manifest is a batch import's repository list (IMPT-003): one file
// naming every repository to migrate in one `import` run.
type Manifest struct {
	Repos []ManifestEntry `yaml:"repos"`
}

// ParseManifest reads a Manifest from YAML.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse import manifest: %w", err)
	}
	if len(m.Repos) == 0 {
		return Manifest{}, fmt.Errorf("import manifest has no repositories")
	}
	for i, entry := range m.Repos {
		if strings.TrimSpace(entry.Source) == "" {
			return Manifest{}, fmt.Errorf("import manifest repo %d: source is required", i+1)
		}
	}
	return m, nil
}

// RepoOptions builds one Options per manifest entry, layering each
// entry's fields over defaults: the batch-wide target instance,
// credentials, and the CLI's -service/-owner/-private/-mirror/
// -mirror-interval flags used as the default whenever an entry doesn't
// set its own.
func (m Manifest) RepoOptions(defaults Options) ([]Options, error) {
	out := make([]Options, 0, len(m.Repos))
	for i, entry := range m.Repos {
		opts := defaults
		opts.SourceURL = entry.Source
		if entry.Service != "" {
			opts.Service = entry.Service
		}
		if entry.Owner != "" {
			opts.RepoOwner = entry.Owner
		}
		if entry.Name != "" {
			opts.RepoName = entry.Name
		}
		if entry.Private != nil {
			opts.Private = *entry.Private
		}
		if entry.Mirror != nil {
			opts.Mirror = *entry.Mirror
		}
		if entry.MirrorInterval != "" {
			d, err := time.ParseDuration(entry.MirrorInterval)
			if err != nil {
				return nil, fmt.Errorf("import manifest repo %d: parse mirrorInterval: %w", i+1, err)
			}
			opts.MirrorInterval = d
		}
		if strings.TrimSpace(opts.RepoOwner) == "" {
			return nil, fmt.Errorf("import manifest repo %d: owner is required (set -owner or the entry's own owner field)", i+1)
		}
		out = append(out, opts)
	}
	return out, nil
}
