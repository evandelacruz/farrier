package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/caddy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// backupHost is what a /backup job needs from a dialed forge host: Run to
// drive every SSH-backed exporter and the push hold (state.Runner), Target
// to build state.SSHGitExporter's ssh:// remote URLs from the same address
// the connection dialed, and Close, since the API dials one connection per
// request and must release it when the job finishes. It is deliberately
// its own interface rather than an addition to Host (server.go): Host is
// shared by /up, /import (indirectly), and /status, and none of them need
// Target — widening it here would mean updating every existing fake that
// satisfies it for no reason those handlers care about.
type backupHost interface {
	state.Runner
	Target() orchestrate.Target
	Close() error
}

// backupRequest is the POST /backup body, one field per the `backup` CLI
// command's flags (cmd/farrier/backup.go).
type backupRequest struct {
	BundleDir      string `json:"bundleDir"`
	Target         string `json:"target"`
	To             string `json:"to"`
	RemoteDir      string `json:"remoteDir,omitempty"`
	WorkDir        string `json:"workDir,omitempty"`
	SSHKeyFile     string `json:"sshKeyFile,omitempty"`
	KnownHostsFile string `json:"knownHostsFile,omitempty"`
	SSHTimeout     string `json:"sshTimeout,omitempty"`
}

// handleBackup implements POST /backup (API-001, BKUP-006): it loads the
// bundle, dials the target, and starts a job running backup.Backup — the
// same function the `backup` CLI command calls — returning the job ID
// immediately (API-002).
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	var req backupRequest
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
	if workDir == "" {
		dir, err := os.MkdirTemp("", "farrier-backup-*")
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("create work directory: %w", err))
			return
		}
		workDir = dir
	}

	job := s.jobs.New()
	go s.runBackup(job, b, req.Target, req.To, remoteDir, workDir, orchestrate.Options{
		KeyFile:        req.SSHKeyFile,
		KnownHostsFile: req.KnownHostsFile,
		Timeout:        timeout,
	})
	writeJobAccepted(w, job.ID())
}

// runBackup dials target, resolves the bundle's keystore and blob drivers
// and age backup key, and wires the SSH-backed state exporters and push
// hold backup.Backup needs, reporting failures on job directly since they
// happen before Backup — which owns the job's terminal event on every
// other path — is even called.
func (s *Server) runBackup(job *events.Job, b *bundle.Bundle, target, destination, remoteDir, workDir string, dialOpts orchestrate.Options) {
	ctx := context.Background()
	host, err := s.dialSSH(ctx, target, dialOpts)
	if err != nil {
		job.Failed(fmt.Sprintf("connect to %s: %v", target, err))
		return
	}
	defer host.Close()

	keystoreDriver, err := s.newKeystore(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		job.Failed(fmt.Sprintf("build keystore driver: %v", err))
		return
	}

	blobAdapter, err := s.newBlob(b.Manifest.Drivers.Blob.Driver, b.Manifest.Drivers.Blob.Config)
	if err != nil {
		job.Failed(fmt.Sprintf("build blob driver: %v", err))
		return
	}

	identity, err := backup.ResolveIdentity(ctx, keystoreDriver)
	if err != nil {
		job.Failed(err.Error())
		return
	}

	t := host.Target()
	opts := backup.Options{
		WorkDir:        workDir,
		ForgejoVersion: b.Manifest.Images[forge.Service],
		Destination:    destination,
		Identity:       identity,
		Git: &state.SSHGitExporter{
			Runner: host,
			User:   t.User,
			Host:   t.Host,
			Port:   t.Port,
			Root:   path.Join(remoteDir, "state", "git"),
		},
		GitCapturer: backup.SSHGitCapturer{Runner: host},
		Database: &state.SSHDatabaseExporter{
			Runner:    host,
			Container: "farrier-" + forge.Service,
			Path:      forge.DatabasePath,
		},
		Blobs: blobAdapter,
		Keys:  &state.KeystoreKeyExporter{Driver: keystoreDriver},
		PushHold: backup.CaddyPushHold{
			Runner:    host,
			Container: "farrier-" + caddy.Service,
			Domain:    b.Manifest.Domain,
			Upstream:  fmt.Sprintf("%s:%d", forge.Service, forge.HTTPPort),
		},
	}
	s.backupRun(ctx, job, opts)
}
