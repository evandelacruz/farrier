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
	"github.com/evandelacruz/farrier/internal/core/upgrade"
)

// upgradeHost is what a /upgrade job needs from a dialed forge host:
// exactly upgrade.Host (deploy.Host plus Target, which the pre-upgrade
// backup's BuildOptions needs), plus Close, since the API dials one
// connection per request and must release it when the job finishes. It is
// its own interface for the same reason backupHost is: Host (server.go) is
// shared by /up, /import, and /status, none of which need Target.
type upgradeHost interface {
	upgrade.Host
	Close() error
}

// upgradeRequest is the POST /upgrade body, one field per the `upgrade` CLI
// command's flags (cmd/farrier/upgrade.go).
type upgradeRequest struct {
	BundleDir      string `json:"bundleDir"`
	Target         string `json:"target"`
	To             string `json:"to"`
	Image          string `json:"image"`
	RemoteDir      string `json:"remoteDir,omitempty"`
	WorkDir        string `json:"workDir,omitempty"`
	DiskPath       string `json:"diskPath,omitempty"`
	SSHKeyFile     string `json:"sshKeyFile,omitempty"`
	KnownHostsFile string `json:"knownHostsFile,omitempty"`
	SSHTimeout     string `json:"sshTimeout,omitempty"`
}

// handleUpgrade implements POST /upgrade (API-001, UPGR-001): it loads the
// bundle, dials the target, and starts a job running upgrade.Upgrade — the
// same function the `upgrade` CLI command calls — returning the job ID
// immediately (API-002).
func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	var req upgradeRequest
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
	if strings.TrimSpace(req.To) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("to is required"))
		return
	}
	if strings.TrimSpace(req.Image) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("image is required"))
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
		dir, err := os.MkdirTemp("", "farrier-upgrade-*")
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("create work directory: %w", err))
			return
		}
		workDir = dir
	}

	job := s.jobs.New()
	go s.runUpgrade(job, b, req.BundleDir, req.Target, req.To, req.Image, remoteDir, workDir, req.DiskPath, autoWorkDir, orchestrate.Options{
		KeyFile:        req.SSHKeyFile,
		KnownHostsFile: req.KnownHostsFile,
		Timeout:        timeout,
	})
	writeJobAccepted(w, job.ID())
}

// runUpgrade dials target, resolves the bundle's keystore and blob drivers
// and age backup key, and hands them to upgrade.Options, reporting failures
// on job directly since they happen before upgrade.Upgrade — which owns the
// job's terminal event on every other path — is even called. If
// autoWorkDir is set (handleUpgrade generated workDir itself rather than
// the operator naming one), runUpgrade removes it on every path that
// returns before upgrade.Upgrade takes ownership of cleaning it up.
func (s *Server) runUpgrade(job *events.Job, b *bundle.Bundle, bundleDir, target, destination, newImage, remoteDir, workDir, diskPath string, autoWorkDir bool, dialOpts orchestrate.Options) {
	ctx := context.Background()
	failWith := func(format string, args ...any) {
		if autoWorkDir {
			os.RemoveAll(workDir)
		}
		job.Failed(fmt.Sprintf(format, args...))
	}

	host, err := s.dialUpgrade(ctx, target, dialOpts)
	if err != nil {
		failWith("connect to %s: %v", target, err)
		return
	}
	defer host.Close()

	keystoreDriver, err := s.newKeystore(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		failWith("build keystore driver: %v", err)
		return
	}

	blobAdapter, err := s.newBlob(b.Manifest.Drivers.Blob.Driver, b.Manifest.Drivers.Blob.Config)
	if err != nil {
		failWith("build blob driver: %v", err)
		return
	}

	identity, err := backup.ResolveIdentity(ctx, keystoreDriver)
	if err != nil {
		failWith("%v", err)
		return
	}

	opts := upgrade.Options{
		BundleDir:   bundleDir,
		RemoteDir:   remoteDir,
		WorkDir:     workDir,
		Bundle:      b,
		Destination: destination,
		NewImage:    newImage,
		Identity:    identity,
		Keystore:    keystoreDriver,
		Blobs:       blobAdapter,
		Host:        host,
		DiskPath:    diskPath,
	}
	s.upgradeRun(ctx, job, opts)
}
