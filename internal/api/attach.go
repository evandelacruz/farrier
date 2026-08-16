package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/attach"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// attachRequest is the POST /attach body, one field per the `attach` CLI
// command's flags. The domain, the DNS-01 provider, and the address the
// instance is served at today are all validated by attach.Attach rather
// than here — whether a bundle may be named at all is a core decision, and
// both frontends get the same refusal from it.
type attachRequest struct {
	BundleDir       string `json:"bundleDir"`
	Target          string `json:"target"`
	RemoteDir       string `json:"remoteDir,omitempty"`
	Domain          string `json:"domain"`
	ACMEDNSProvider string `json:"acmeDnsProvider"`
	ACMEEmail       string `json:"acmeEmail,omitempty"`
	ACMEDirectory   string `json:"acmeDirectory,omitempty"`
	Address         string `json:"address"`
	SSHKeyFile      string `json:"sshKeyFile,omitempty"`
	KnownHostsFile  string `json:"knownHostsFile,omitempty"`
	SSHTimeout      string `json:"sshTimeout,omitempty"`
}

// handleAttach implements POST /attach (API-001, UP-007): it loads the
// nameless bundle, dials the host it runs on, and starts a job running
// attach.Attach — the same function the `attach` CLI command calls —
// returning the job ID immediately (API-002). The clone URLs that changed
// arrive on that job's event stream, so the dashboard renders exactly what
// the CLI prints.
func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	var req attachRequest
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
	go s.runAttach(job, b, remoteDir, req, orchestrate.Options{
		KeyFile:        req.SSHKeyFile,
		KnownHostsFile: req.KnownHostsFile,
		Timeout:        timeout,
	})
	writeJobAccepted(w, job.ID())
}

// runAttach dials the host and names b's instance on it, reporting
// connection failures on job directly since they happen before
// attach.Attach — which owns the job's terminal event on every other path
// — is even called.
func (s *Server) runAttach(job *events.Job, b *bundle.Bundle, remoteDir string, req attachRequest, dialOpts orchestrate.Options) {
	ctx := context.Background()
	host, err := s.dial(ctx, req.Target, dialOpts)
	if err != nil {
		job.Failed(fmt.Sprintf("connect to %s: %v", req.Target, err))
		return
	}
	defer host.Close()

	_, _ = s.attachRun(ctx, job, attach.Options{
		BundleDir:       req.BundleDir,
		RemoteDir:       remoteDir,
		Bundle:          b,
		Host:            host,
		Domain:          req.Domain,
		ACMEDNSProvider: req.ACMEDNSProvider,
		ACMEEmail:       req.ACMEEmail,
		ACMEDirectory:   req.ACMEDirectory,
		Address:         req.Address,
	})
}
