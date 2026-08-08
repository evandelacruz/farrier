package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// defaultRemoteDir matches the `up` CLI command's -remote-dir default
// (cmd/farrier/main.go), so a bare {"bundleDir":...,"target":...} request
// deploys to the same place the CLI would.
const defaultRemoteDir = "/opt/farrier"

// upRequest is the POST /up body, one field per the `up` CLI command's
// flags.
type upRequest struct {
	BundleDir      string `json:"bundleDir"`
	Target         string `json:"target"`
	RemoteDir      string `json:"remoteDir,omitempty"`
	SSHKeyFile     string `json:"sshKeyFile,omitempty"`
	KnownHostsFile string `json:"knownHostsFile,omitempty"`
	SSHTimeout     string `json:"sshTimeout,omitempty"`
}

// handleUp implements POST /up (API-001, UP-001..002): it loads the
// bundle, dials the target, and starts a job running deploy.Up — the same
// function the `up` CLI command calls — returning the job ID immediately
// (API-002).
func (s *Server) handleUp(w http.ResponseWriter, r *http.Request) {
	var req upRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode request: %w", err))
		return
	}
	if strings.TrimSpace(req.BundleDir) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bundleDir is required"))
		return
	}
	if strings.TrimSpace(req.Target) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("target is required"))
		return
	}

	var timeout time.Duration
	if req.SSHTimeout != "" {
		d, err := time.ParseDuration(req.SSHTimeout)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("sshTimeout: %w", err))
			return
		}
		timeout = d
	}

	b, err := s.loadBundle(req.BundleDir)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("load bundle: %w", err))
		return
	}

	remoteDir := req.RemoteDir
	if remoteDir == "" {
		remoteDir = defaultRemoteDir
	}

	job := s.jobs.New()
	go s.runUp(job, b, req.Target, remoteDir, orchestrate.Options{
		KeyFile:        req.SSHKeyFile,
		KnownHostsFile: req.KnownHostsFile,
		Timeout:        timeout,
	})
	writeJobAccepted(w, job.ID())
}

// runUp dials target and deploys b to it, reporting connection failures on
// job directly since they happen before deploy.Up — which owns the job's
// terminal event on every other path — is even called.
func (s *Server) runUp(job *events.Job, b *bundle.Bundle, target, remoteDir string, dialOpts orchestrate.Options) {
	ctx := context.Background()
	host, err := s.dial(ctx, target, dialOpts)
	if err != nil {
		job.Failed(fmt.Sprintf("connect to %s: %v", target, err))
		return
	}
	defer host.Close()
	s.deployUp(ctx, job, host, b, deploy.Options{RemoteDir: remoteDir})
}
