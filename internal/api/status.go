package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/status"
)

// statusResponse is GET /status's body: status.Report reshaped with
// camelCase JSON field names, matching the request-side convention every
// other verb in this package already uses. Field order mirrors
// status.Report.
type statusResponse struct {
	Services []serviceStatusResponse `json:"services"`
	TLS      tlsStatusResponse       `json:"tls"`
	Disk     diskStatusResponse      `json:"disk"`
	Lag      lagResponse             `json:"lag"`
}

type serviceStatusResponse struct {
	Name   string `json:"name"`
	Up     bool   `json:"up"`
	Detail string `json:"detail"`
}

type tlsStatusResponse struct {
	NotAfter     time.Time `json:"notAfter"`
	Valid        bool      `json:"valid"`
	ExpiringSoon bool      `json:"expiringSoon"`
}

type diskStatusResponse struct {
	Path           string `json:"path"`
	TotalBytes     uint64 `json:"totalBytes"`
	UsedBytes      uint64 `json:"usedBytes"`
	AvailableBytes uint64 `json:"availableBytes"`
}

type lagResponse struct {
	State      status.LagState `json:"state"`
	LastBackup time.Time       `json:"lastBackup,omitempty"`
	Age        string          `json:"age,omitempty"`
	Skew       string          `json:"skew,omitempty"`
}

func newStatusResponse(r status.Report) statusResponse {
	services := make([]serviceStatusResponse, len(r.Services))
	for i, svc := range r.Services {
		services[i] = serviceStatusResponse{Name: svc.Name, Up: svc.Up, Detail: svc.Detail}
	}
	resp := statusResponse{
		Services: services,
		TLS: tlsStatusResponse{
			NotAfter:     r.TLS.NotAfter,
			Valid:        r.TLS.Valid,
			ExpiringSoon: r.TLS.ExpiringSoon,
		},
		Disk: diskStatusResponse{
			Path:           r.Disk.Path,
			TotalBytes:     r.Disk.TotalBytes,
			UsedBytes:      r.Disk.UsedBytes,
			AvailableBytes: r.Disk.AvailableBytes,
		},
		Lag: lagResponse{State: r.Lag.State},
	}
	if r.Lag.State == status.LagMeasured {
		resp.Lag.LastBackup = r.Lag.LastBackup
		resp.Lag.Age = r.Lag.Age.String()
		resp.Lag.Skew = r.Lag.Skew.String()
	}
	return resp
}

// handleStatus implements GET /status (API-001, STAT-001..002): unlike
// every mutation verb, status is a read — it dials the target, builds the
// bundle's keystore driver, and calls status.Check synchronously, the same
// as the `status` CLI command, returning the report directly instead of a
// job ID.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bundleDir := q.Get("bundleDir")
	target := q.Get("target")
	if strings.TrimSpace(bundleDir) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bundleDir is required"))
		return
	}
	if strings.TrimSpace(target) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("target is required"))
		return
	}

	remoteDir := q.Get("remoteDir")
	if remoteDir == "" {
		remoteDir = defaultRemoteDir
	}
	diskPath := q.Get("diskPath")
	if diskPath == "" {
		diskPath = status.DefaultDiskPath
	}

	var timeout time.Duration
	if v := q.Get("sshTimeout"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("sshTimeout: %w", err))
			return
		}
		timeout = d
	}

	b, err := s.loadBundle(bundleDir)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("load bundle: %w", err))
		return
	}

	driver, err := s.newKeystore(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("build keystore driver: %w", err))
		return
	}

	ctx := r.Context()
	host, err := s.dial(ctx, target, orchestrate.Options{
		KeyFile:        q.Get("sshKeyFile"),
		KnownHostsFile: q.Get("knownHostsFile"),
		Timeout:        timeout,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("connect to %s: %w", target, err))
		return
	}
	defer host.Close()

	report, err := s.statusCheck(ctx, status.Options{
		Runner:    host,
		Bundle:    b,
		RemoteDir: remoteDir,
		Keystore:  driver,
		DiskPath:  diskPath,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(newStatusResponse(report))
}
