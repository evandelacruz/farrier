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
	"github.com/evandelacruz/farrier/internal/core/promote"
)

// promoteRequest is the POST /promote body, one field per the `promote`
// CLI command's flags (cmd/farrier/promote.go), plus Confirm (FAIL-002):
// unlike the CLI, this endpoint can't prompt on a terminal, so Confirm is
// the operator's explicit stand-in for the interactive "type yes" prompt.
// It defaults to false, so an unconfirmed request is refused rather than
// silently promoting.
type promoteRequest struct {
	BundleDir      string `json:"bundleDir"`
	Target         string `json:"target"`
	From           string `json:"from"`
	Snapshot       string `json:"snapshot,omitempty"`
	RemoteDir      string `json:"remoteDir,omitempty"`
	WorkDir        string `json:"workDir,omitempty"`
	SSHKeyFile     string `json:"sshKeyFile,omitempty"`
	KnownHostsFile string `json:"knownHostsFile,omitempty"`
	SSHTimeout     string `json:"sshTimeout,omitempty"`
	DNSRecord      string `json:"dnsRecord,omitempty"`
	DNSValue       string `json:"dnsValue,omitempty"`
	Confirm        bool   `json:"confirm,omitempty"`
}

// handlePromote implements POST /promote (API-001, FAIL-001): it loads the
// bundle, dials the standby target, and starts a job running
// promote.Promote — the same function the `promote` CLI command calls —
// returning the job ID immediately (API-002).
func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request) {
	var req promoteRequest
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

	source, err := backup.OpenDestination(req.From)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("open snapshot source: %w", err))
		return
	}
	resolvedKey, age, err := backup.SnapshotAge(r.Context(), source, req.Snapshot, time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("resolve snapshot: %w", err))
		return
	}
	if !req.Confirm {
		writeError(w, http.StatusBadRequest, fmt.Errorf("confirm is required: snapshot %s is %s old; resubmit with confirm: true to promote it", resolvedKey, age.Round(time.Second)))
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

	dnsValue := req.DNSValue
	if dnsValue == "" {
		parsed, err := orchestrate.ParseTarget(req.Target)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("target: %w", err))
			return
		}
		dnsValue = parsed.Host
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
		dir, err := os.MkdirTemp("", "farrier-promote-*")
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("create work directory: %w", err))
			return
		}
		workDir = dir
	}

	job := s.jobs.New()
	go s.runPromote(job, b, req.Target, req.From, resolvedKey, remoteDir, workDir, autoWorkDir, req.DNSRecord, dnsValue, orchestrate.Options{
		KeyFile:        req.SSHKeyFile,
		KnownHostsFile: req.KnownHostsFile,
		Timeout:        timeout,
	})
	writeJobAccepted(w, job.ID())
}

// runPromote dials target, resolves the bundle's keystore, blob, and DNS
// drivers, the snapshot source, and the age backup key, and hands them to
// promote.Options, reporting failures on job directly since they happen
// before promote.Promote — which owns the job's terminal event on every
// other path — is even called. If autoWorkDir is set (handlePromote
// generated workDir itself rather than the operator naming one), runPromote
// removes it on every path that returns before promote.Promote takes
// ownership of cleaning it up.
func (s *Server) runPromote(job *events.Job, b *bundle.Bundle, target, from, snapshotKey, remoteDir, workDir string, autoWorkDir bool, dnsRecord, dnsValue string, dialOpts orchestrate.Options) {
	ctx := context.Background()
	failWith := func(format string, args ...any) {
		if autoWorkDir {
			os.RemoveAll(workDir)
		}
		job.Failed(fmt.Sprintf(format, args...))
	}

	host, err := s.dialPromote(ctx, target, dialOpts)
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

	dnsDriver, err := s.resolveDNS(ctx, job, b.Manifest.Drivers.DNS, keystoreDriver)
	if err != nil {
		failWith("build dns driver: %v", err)
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

	opts := promote.Options{
		RemoteDir:   remoteDir,
		WorkDir:     workDir,
		Bundle:      b,
		Source:      source,
		SnapshotKey: snapshotKey,
		Identity:    identity,
		Keystore:    keystoreDriver,
		Blobs:       blobAdapter,
		Host:        host,
		DNS:         dnsDriver,
		DNSRecord:   dnsRecord,
		DNSValue:    dnsValue,
	}
	s.promoteRun(ctx, job, opts)
}
