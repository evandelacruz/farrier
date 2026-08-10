package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/publish"
)

// publishRequest is the POST /publish body (IMPT-004). Dir is the project
// folder to publish; every other field is an override of a default that
// already points at that folder's own bundle and instance, mirroring the
// `publish` CLI command's flags.
//
// The instance token is deliberately not a request field: like the CLI, it
// comes from FARRIER_TARGET_TOKEN in the server process's own environment,
// never the wire.
type publishRequest struct {
	Dir       string `json:"dir"`
	Bundle    string `json:"bundle,omitempty"`
	Target    string `json:"target,omitempty"`
	Owner     string `json:"owner,omitempty"`
	Name      string `json:"name,omitempty"`
	Private   *bool  `json:"private,omitempty"`
	Remote    string `json:"remote,omitempty"`
	PublicKey string `json:"publicKey,omitempty"`
}

// handlePublish implements POST /publish (API-001, IMPT-004): it starts a
// job running publish.Run — the same function the `publish` CLI command
// calls — and returns the job ID immediately (API-002).
//
// The folder and the git it runs are the server's own, which is the point:
// the API listens on loopback on the operator's machine, so "the local
// project folder" means the same thing to both frontends.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	var req publishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode request: %w", err))
		return
	}
	if strings.TrimSpace(req.Dir) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("dir is required"))
		return
	}

	token := os.Getenv("FARRIER_TARGET_TOKEN")
	if strings.TrimSpace(token) == "" {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("FARRIER_TARGET_TOKEN must be set in the server environment"))
		return
	}

	bundleDir := strings.TrimSpace(req.Bundle)
	if bundleDir == "" {
		bundleDir = bundle.DirFor(req.Dir)
	}
	b, err := s.loadBundle(bundleDir)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("load bundle: %w", err))
		return
	}

	private := true
	if req.Private != nil {
		private = *req.Private
	}

	job := s.jobs.New()
	go s.publishRun(context.Background(), job, publish.Options{
		Dir:           req.Dir,
		Manifest:      &b.Manifest,
		TargetBaseURL: req.Target,
		TargetToken:   keystore.NewSecret(token),
		Owner:         req.Owner,
		Name:          req.Name,
		Private:       private,
		RemoteName:    req.Remote,
		PublicKeyPath: req.PublicKey,
	})
	writeJobAccepted(w, job.ID())
}
