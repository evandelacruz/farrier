// Package api implements internal/api (tech-spec.md "API"): a loopback
// HTTP server exposing RPC verbs for core operations (API-001). Every
// mutation verb starts a CORE-002 job and returns its ID immediately;
// GET /jobs/{id}/events streams that job's progress over SSE (API-002).
// Frontends contain zero logic (CLAUDE.md "one core, thin skins"): every
// handler here does request parsing and job wiring only, calling straight
// into the same core functions the CLI calls.
package api

import (
	"context"
	"net/http"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/dns"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/importer"
	"github.com/evandelacruz/farrier/internal/core/initialize"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/promote"
	"github.com/evandelacruz/farrier/internal/core/restore"
	"github.com/evandelacruz/farrier/internal/core/status"
	"github.com/evandelacruz/farrier/internal/core/upgrade"
)

// DefaultAddr is the loopback address the API binds by default
// (tech-spec.md "API"). Exposing it beyond loopback — VPN, tailnet — is
// the operator's own topology (spec.md "Interfaces"); ListenAndServe
// accepts any addr for that purpose.
const DefaultAddr = "127.0.0.1:7433"

// Host is what an /up job needs from a dialed deployment target: exactly
// deploy.Host, the interface deploy.Up itself requires, plus Close, since
// the API dials one connection per request and must release it when the
// job finishes.
type Host interface {
	deploy.Host
	Close() error
}

// Server is the loopback RPC server. Every core call it makes is behind a
// replaceable field so tests can drive request parsing, job wiring,
// response codes, and SSE framing without a real ACME exchange or SSH
// connection; New returns a Server wired to the real core implementations.
type Server struct {
	jobs *events.Store

	initRun        func(ctx context.Context, job *events.Job, params initialize.Params) (*bundle.Bundle, error)
	loadBundle     func(dir string) (*bundle.Bundle, error)
	dial           func(ctx context.Context, target string, opts orchestrate.Options) (Host, error)
	deployUp       func(ctx context.Context, job *events.Job, host deploy.Host, b *bundle.Bundle, opts deploy.Options) error
	importRun      func(ctx context.Context, job *events.Job, opts importer.Options) (importer.Result, error)
	importRunBatch func(ctx context.Context, job *events.Job, opts importer.BatchOptions) (importer.BatchResult, error)
	statusCheck    func(ctx context.Context, opts status.Options) (status.Report, error)
	newKeystore    func(driverName string, config map[string]any) (keystore.Driver, error)
	newBlob        func(driverName string, config map[string]any) (blob.Adapter, error)
	dialSSH        func(ctx context.Context, target string, opts orchestrate.Options) (backupHost, error)
	backupRun      func(ctx context.Context, job *events.Job, opts backup.Options) (string, error)
	dialRestore    func(ctx context.Context, target string, opts orchestrate.Options) (restore.Host, error)
	restoreRun     func(ctx context.Context, job *events.Job, opts restore.Options) error
	dialPromote    func(ctx context.Context, target string, opts orchestrate.Options) (promote.Host, error)
	resolveDNS     func(ctx context.Context, job *events.Job, ref bundle.DriverRef, keystoreDriver keystore.Driver) (dns.Driver, error)
	promoteRun     func(ctx context.Context, job *events.Job, opts promote.Options) error
	dialUpgrade    func(ctx context.Context, target string, opts orchestrate.Options) (upgradeHost, error)
	upgradeRun     func(ctx context.Context, job *events.Job, opts upgrade.Options) error
}

// New returns a Server wired to the real core implementations: the same
// initialize.Run, bundle.Load, orchestrate.Connect, deploy.Up,
// importer.Run/RunBatch, status.Check, and keystore.New the CLI calls
// directly.
func New() *Server {
	return &Server{
		jobs:       events.NewStore(),
		initRun:    initialize.Run,
		loadBundle: bundle.Load,
		dial: func(ctx context.Context, target string, opts orchestrate.Options) (Host, error) {
			return orchestrate.Connect(ctx, target, opts)
		},
		deployUp:       deploy.Up,
		importRun:      importer.Run,
		importRunBatch: importer.RunBatch,
		statusCheck:    status.Check,
		newKeystore:    keystore.New,
		newBlob:        blob.New,
		dialSSH: func(ctx context.Context, target string, opts orchestrate.Options) (backupHost, error) {
			return orchestrate.Connect(ctx, target, opts)
		},
		backupRun: backup.Backup,
		dialRestore: func(ctx context.Context, target string, opts orchestrate.Options) (restore.Host, error) {
			return orchestrate.Connect(ctx, target, opts)
		},
		restoreRun: restore.Restore,
		dialPromote: func(ctx context.Context, target string, opts orchestrate.Options) (promote.Host, error) {
			return orchestrate.Connect(ctx, target, opts)
		},
		resolveDNS: promote.ResolveDNSDriver,
		promoteRun: promote.Promote,
		dialUpgrade: func(ctx context.Context, target string, opts orchestrate.Options) (upgradeHost, error) {
			return orchestrate.Connect(ctx, target, opts)
		},
		upgradeRun: upgrade.Upgrade,
	}
}

// Handler returns the server's routed http.Handler: POST /init, POST /up,
// POST /import, POST /backup, POST /restore, POST /promote,
// POST /upgrade, GET /status, and GET /jobs/{id}/events, the RPC verbs for
// every core operation currently implemented (tech-spec.md "API"). Verbs
// for operations not yet landed in internal/core (drill) are added here as
// each one lands.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /init", s.handleInit)
	mux.HandleFunc("POST /up", s.handleUp)
	mux.HandleFunc("POST /import", s.handleImport)
	mux.HandleFunc("POST /backup", s.handleBackup)
	mux.HandleFunc("POST /restore", s.handleRestore)
	mux.HandleFunc("POST /promote", s.handlePromote)
	mux.HandleFunc("POST /upgrade", s.handleUpgrade)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /jobs/{id}/events", s.handleJobEvents)
	return mux
}

// ListenAndServe starts the RPC server on addr, or DefaultAddr if addr is
// empty (API-001: loopback by default).
func (s *Server) ListenAndServe(addr string) error {
	if addr == "" {
		addr = DefaultAddr
	}
	return http.ListenAndServe(addr, s.Handler())
}
