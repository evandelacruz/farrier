package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/importer"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// defaultMirrorInterval matches the `import` CLI command's -mirror-interval
// default (cmd/farrier/import.go), so a request that sets "mirror": true
// without an explicit "mirrorInterval" gets the same re-sync cadence the
// CLI would give it.
const defaultMirrorInterval = 8 * time.Hour

// importRequest is the POST /import body. Exactly one of Source or Repos
// is required: Source imports a single repository (importer.Run), Repos
// imports a batch (importer.RunBatch), the same split as the `import`
// CLI command's -source/-file. Service, Owner, Name, Private, Mirror, and
// MirrorInterval are the batch-wide defaults a Repos entry can override,
// mirroring the CLI's -service/-owner/-private/-mirror/-mirror-interval
// flags; for a single Source import they simply are that repository's
// values.
//
// TargetToken and SourceToken are deliberately not request fields: like
// the CLI, they come from FARRIER_TARGET_TOKEN and FARRIER_SOURCE_TOKEN in
// the server process's own environment, never the wire, so neither token
// ever needs to travel as a JSON value (tech-spec.md "Importing
// repositories").
type importRequest struct {
	Target string                   `json:"target"`
	Source string                   `json:"source,omitempty"`
	Repos  []importer.ManifestEntry `json:"repos,omitempty"`

	Service        string `json:"service,omitempty"`
	Owner          string `json:"owner,omitempty"`
	Name           string `json:"name,omitempty"`
	Private        *bool  `json:"private,omitempty"`
	Mirror         *bool  `json:"mirror,omitempty"`
	MirrorInterval string `json:"mirrorInterval,omitempty"`
}

// handleImport implements POST /import (API-001, IMPT-001..003): it starts
// a job running importer.Run for a single repository or importer.RunBatch
// for a Repos list — the same functions the `import` CLI command calls —
// and returns the job ID immediately (API-002).
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var req importRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode request: %w", err))
		return
	}
	if strings.TrimSpace(req.Target) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("target is required"))
		return
	}
	hasSource := strings.TrimSpace(req.Source) != ""
	hasRepos := len(req.Repos) > 0
	if hasSource == hasRepos {
		writeError(w, http.StatusBadRequest, fmt.Errorf("exactly one of source or repos is required"))
		return
	}

	targetToken := os.Getenv("FARRIER_TARGET_TOKEN")
	sourceToken := os.Getenv("FARRIER_SOURCE_TOKEN")
	if strings.TrimSpace(targetToken) == "" {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("FARRIER_TARGET_TOKEN must be set in the server environment"))
		return
	}
	if strings.TrimSpace(sourceToken) == "" {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("FARRIER_SOURCE_TOKEN must be set in the server environment"))
		return
	}

	private := true
	if req.Private != nil {
		private = *req.Private
	}
	mirror := false
	if req.Mirror != nil {
		mirror = *req.Mirror
	}

	defaults := importer.Options{
		TargetBaseURL: req.Target,
		TargetToken:   keystore.NewSecret(targetToken),
		SourceToken:   keystore.NewSecret(sourceToken),
		Service:       req.Service,
		RepoOwner:     req.Owner,
		Private:       private,
		Mirror:        mirror,
	}
	if mirror {
		defaults.MirrorInterval = defaultMirrorInterval
		if req.MirrorInterval != "" {
			d, err := time.ParseDuration(req.MirrorInterval)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Errorf("mirrorInterval: %w", err))
				return
			}
			defaults.MirrorInterval = d
		}
	}

	entries := req.Repos
	if hasSource {
		// Service, Owner, Private, and Mirror are already in defaults
		// above; only Source and Name are per-repository here since
		// there's exactly one repository.
		entries = []importer.ManifestEntry{{Source: req.Source, Name: req.Name}}
	}
	repoOpts, err := importer.Manifest{Repos: entries}.RepoOptions(defaults)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	job := s.jobs.New()
	if hasRepos {
		go s.importRunBatch(context.Background(), job, importer.BatchOptions{Repos: repoOpts})
	} else {
		go s.importRun(context.Background(), job, repoOpts[0])
	}
	writeJobAccepted(w, job.ID())
}
