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
	"github.com/evandelacruz/farrier/internal/core/drill"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// drillRequest is the POST /drill body, one field per the `drill` CLI
// command's flags (cmd/farrier/drill.go). There is no snapshot field, for
// the same reason the CLI has no -snapshot flag: a drill rehearses the most
// recent backup (DRIL-001).
type drillRequest struct {
	BundleDir      string `json:"bundleDir"`
	Target         string `json:"target"`
	From           string `json:"from"`
	RemoteDir      string `json:"remoteDir,omitempty"`
	WorkDir        string `json:"workDir,omitempty"`
	SSHKeyFile     string `json:"sshKeyFile,omitempty"`
	KnownHostsFile string `json:"knownHostsFile,omitempty"`
	SSHTimeout     string `json:"sshTimeout,omitempty"`
}

// handleDrill implements POST /drill (API-001, DRIL-001): it loads the
// bundle, dials the scratch target, and starts a job running drill.Drill —
// the same function the `drill` CLI command calls — returning the job ID
// immediately (API-002). The drill's report reaches the caller as the job's
// event stream: on failure its terminal event names the specific step that
// failed.
//
// Unlike POST /promote, this verb needs no confirmation field: a drill acts
// only on a scratch target and never touches DNS, so there is nothing here
// for an unconfirmed request to damage.
func (s *Server) handleDrill(w http.ResponseWriter, r *http.Request) {
	var req drillRequest
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
		dir, err := os.MkdirTemp("", "farrier-drill-*")
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("create work directory: %w", err))
			return
		}
		workDir = dir
	}

	job := s.jobs.New()
	go s.runDrill(job, b, req.Target, req.From, remoteDir, workDir, autoWorkDir, orchestrate.Options{
		KeyFile:        req.SSHKeyFile,
		KnownHostsFile: req.KnownHostsFile,
		Timeout:        timeout,
	})
	writeJobAccepted(w, job.ID())
}

// runDrill dials the scratch target, resolves the bundle's keystore and
// blob drivers, the snapshot source, and the age backup key, and hands them
// to drill.Options, reporting failures on job directly since they happen
// before drill.Drill — which owns the job's terminal event on every other
// path — is even called. If autoWorkDir is set (handleDrill generated
// workDir itself rather than the operator naming one), runDrill removes it
// on every path that returns before drill.Drill takes ownership of cleaning
// it up.
func (s *Server) runDrill(job *events.Job, b *bundle.Bundle, target, from, remoteDir, workDir string, autoWorkDir bool, dialOpts orchestrate.Options) {
	ctx := context.Background()
	failWith := func(format string, args ...any) {
		if autoWorkDir {
			os.RemoveAll(workDir)
		}
		job.Failed(fmt.Sprintf(format, args...))
	}

	host, err := s.dialDrill(ctx, target, dialOpts)
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

	source, err := backup.OpenDestination(from)
	if err != nil {
		failWith("open snapshot source: %v", err)
		return
	}

	identity, err := backup.ResolveIdentity(ctx, keystoreDriver)
	if err != nil {
		failWith("%v", err)
		return
	}

	s.drillRun(ctx, job, drill.Options{
		RemoteDir: remoteDir,
		WorkDir:   workDir,
		Bundle:    b,
		Source:    source,
		Identity:  identity,
		Keystore:  keystoreDriver,
		Blobs:     blobAdapter,
		Host:      host,
	})
}
