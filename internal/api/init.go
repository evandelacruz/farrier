package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/initialize"
)

// initDriverRef is a bundle.DriverRef as it arrives over the wire: driver
// name plus non-secret config (bundle.go "DriverRef" — never a secret's
// value, only a pointer to one).
type initDriverRef struct {
	Driver string         `json:"driver"`
	Config map[string]any `json:"config,omitempty"`
}

// initRequest is the POST /init body, one field per initialize.Params.
type initRequest struct {
	// Domain is optional: omitting it asks for a nameless bundle
	// (INIT-005), which skips zone proof and certificate issuance and
	// requires the operator to own nothing. acmeDnsProvider then has to be
	// omitted too.
	Domain string `json:"domain"`
	// Project is the project folder the forge is for, resolved on the
	// machine running the loopback server — the operator's own, since that
	// is the only place Farrier runs (spec.md "operator's machine is the
	// control plane").
	Project string `json:"project"`
	// Dir optionally overrides the bundle location; empty writes it inside
	// Project (INIT-001).
	Dir             string        `json:"dir"`
	Keystore        initDriverRef `json:"keystore"`
	Blob            initDriverRef `json:"blob"`
	ACMEDNSProvider string        `json:"acmeDnsProvider"`
	ACMEEmail       string        `json:"acmeEmail"`
	// ACMEDirectory is the ACME server to issue against: omitted for Let's
	// Encrypt production, "staging" for its staging environment, or any
	// other ACME directory URL. Like the two fields above it is rejected
	// without a domain, and the resolved URL is recorded in the manifest so
	// renewal reaches the CA that issued.
	ACMEDirectory string            `json:"acmeDirectory,omitempty"`
	Images        map[string]string `json:"images,omitempty"`
	// GitSSHPort is the host port the instance serves git over SSH on
	// (UP-005); omitted or zero takes bundle.DefaultGitSSHPort.
	GitSSHPort int `json:"gitSshPort,omitempty"`
	// WebPort is the host port the instance's web UI is published on
	// (UP-002, UP-006); omitted or zero takes the default for the tier —
	// bundle.DefaultNamedWebPort with a domain, DefaultNamelessWebPort
	// without one.
	WebPort int `json:"webPort,omitempty"`
	// PublicWebPort is the port clients connect on when something on the
	// host holds the standard port and forwards to Farrier; omitted or
	// zero means Caddy is the edge.
	PublicWebPort int `json:"publicWebPort,omitempty"`
}

// handleInit implements POST /init (API-001, INIT-001..003): it starts a
// job running initialize.Run — the same function the `init` CLI command
// calls — and returns the job ID immediately (API-002). initialize.Run
// reports its own validation and progress through the job's event stream;
// handleInit only rejects requests it cannot even parse into params.
func (s *Server) handleInit(w http.ResponseWriter, r *http.Request) {
	var req initRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode request: %w", err))
		return
	}
	if strings.TrimSpace(req.Project) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("project is required"))
		return
	}
	if strings.TrimSpace(req.Keystore.Driver) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("keystore.driver is required"))
		return
	}
	if strings.TrimSpace(req.Blob.Driver) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("blob.driver is required"))
		return
	}
	// Required only with a domain (INIT-005). A provider without one is a
	// contradiction initialize.Run rejects through the event stream, the
	// same as every other params-level disagreement.
	if strings.TrimSpace(req.Domain) != "" && strings.TrimSpace(req.ACMEDNSProvider) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("acmeDnsProvider is required with domain"))
		return
	}
	if err := bundle.ValidateGitSSHPort(req.GitSSHPort); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := bundle.ValidateWebPort(req.WebPort); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := bundle.ValidateWebPort(req.PublicWebPort); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	params := initialize.Params{
		Domain:          req.Domain,
		Project:         req.Project,
		Dir:             req.Dir,
		Keystore:        bundle.DriverRef{Driver: req.Keystore.Driver, Config: req.Keystore.Config},
		Blob:            bundle.DriverRef{Driver: req.Blob.Driver, Config: req.Blob.Config},
		ACMEDNSProvider: req.ACMEDNSProvider,
		ACMEEmail:       req.ACMEEmail,
		ACMEDirectory:   req.ACMEDirectory,
		Images:          req.Images,
		GitSSHPort:      req.GitSSHPort,
		WebPort:         req.WebPort,
		PublicWebPort:   req.PublicWebPort,
	}

	job := s.jobs.New()
	go s.initRun(context.Background(), job, params)
	writeJobAccepted(w, job.ID())
}
