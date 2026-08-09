package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/restore"
)

// restoreRequest is the POST /restore body, one field per the `restore` CLI
// command's flags (cmd/farrier/restore.go).
type restoreRequest struct {
	BundleDir      string `json:"bundleDir"`
	Target         string `json:"target"`
	From           string `json:"from"`
	Snapshot       string `json:"snapshot,omitempty"`
	RemoteDir      string `json:"remoteDir,omitempty"`
	WorkDir        string `json:"workDir,omitempty"`
	SSHKeyFile     string `json:"sshKeyFile,omitempty"`
	KnownHostsFile string `json:"knownHostsFile,omitempty"`
	SSHTimeout     string `json:"sshTimeout,omitempty"`
}

// handleRestore implements POST /restore (API-001, RSTR-001): it loads the
// bundle, dials the target, and starts a job running restore.Restore — the
// same function the `restore` CLI command calls — returning the job ID
// immediately (API-002).
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	var req restoreRequest
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
	if strings.TrimSpace(req.From) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("from is required"))
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
	workDir := req.WorkDir
	autoWorkDir := workDir == ""
	if autoWorkDir {
		dir, err := os.MkdirTemp("", "farrier-restore-*")
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("create work directory: %w", err))
			return
		}
		workDir = dir
	}

	job := s.jobs.New()
	go s.runRestore(job, b, req.Target, req.From, req.Snapshot, remoteDir, workDir, autoWorkDir, orchestrate.Options{
		KeyFile:        req.SSHKeyFile,
		KnownHostsFile: req.KnownHostsFile,
		Timeout:        timeout,
	})
	writeJobAccepted(w, job.ID())
}

// runRestore dials target, resolves the bundle's keystore and blob drivers,
// the snapshot source, and the age backup key, and hands them to
// restore.Options, reporting failures on job directly since they happen
// before restore.Restore — which owns the job's terminal event on every
// other path — is even called. If autoWorkDir is set (handleRestore
// generated workDir itself rather than the operator naming one), runRestore
// removes it on every path that returns before restore.Restore takes
// ownership of cleaning it up.
func (s *Server) runRestore(job *events.Job, b *bundle.Bundle, target, from, snapshotKey, remoteDir, workDir string, autoWorkDir bool, dialOpts orchestrate.Options) {
	ctx := context.Background()
	host, err := s.dialRestore(ctx, target, dialOpts)
	if err != nil {
		if autoWorkDir {
			os.RemoveAll(workDir)
		}
		job.Failed(fmt.Sprintf("connect to %s: %v", target, err))
		return
	}
	defer host.Close()

	keystoreDriver, err := s.newKeystore(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		if autoWorkDir {
			os.RemoveAll(workDir)
		}
		job.Failed(fmt.Sprintf("build keystore driver: %v", err))
		return
	}

	blobAdapter, err := s.newBlob(b.Manifest.Drivers.Blob.Driver, b.Manifest.Drivers.Blob.Config)
	if err != nil {
		if autoWorkDir {
			os.RemoveAll(workDir)
		}
		job.Failed(fmt.Sprintf("build blob driver: %v", err))
		return
	}

	source, err := backup.OpenDestination(from)
	if err != nil {
		if autoWorkDir {
			os.RemoveAll(workDir)
		}
		job.Failed(fmt.Sprintf("open snapshot source: %v", err))
		return
	}

	identity, err := backup.ResolveIdentity(ctx, keystoreDriver)
	if err != nil {
		if autoWorkDir {
			os.RemoveAll(workDir)
		}
		job.Failed(err.Error())
		return
	}

	opts := restore.Options{
		RemoteDir:   remoteDir,
		WorkDir:     workDir,
		Bundle:      b,
		Source:      source,
		SnapshotKey: snapshotKey,
		Identity:    identity,
		Keystore:    keystoreDriver,
		Blobs:       blobAdapter,
		Host:        host,
	}
	s.restoreRun(ctx, job, opts)
}
