// Package status implements the instance-health report STAT-001 requires:
// which of the bundle's deployed services are up, whether its TLS
// certificate is still valid and how close it is to expiry, and how much
// disk headroom the forge host has left.
//
// STAT-001 also requires reporting last-backup age. That part is deferred
// (docs/status.json carries the remaining note): finding the most recent
// snapshot needs a stable convention for what backup writes to its
// destination and how status finds it there, and backup (BKUP-001..005),
// which owns the snapshot format and destination (tech-spec.md "Snapshot
// format"), has not landed. Nothing here invents that convention ahead of
// backup itself defining it.
package status

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/caddy"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// CertExpiryWarning is how close to expiry a TLS certificate must be before
// TLSStatus.ExpiringSoon reports true — the 14-day threshold ACME-002 and
// tech-spec.md "Operational targets" ("Cert renewal") settle.
const CertExpiryWarning = 14 * 24 * time.Hour

// DefaultDiskPath is the filesystem path DiskStatus reports on when Options
// doesn't override it: the host's root filesystem, since none of the
// landed decisions pin forge state to a specific bind-mounted host
// directory yet.
const DefaultDiskPath = "/"

// ServiceStatus reports one Compose service's run state.
type ServiceStatus struct {
	// Name is the Compose service name (e.g. forge.Service, caddy.Service).
	Name string
	// Up is true when docker compose reports the service's container state
	// as "running".
	Up bool
	// Detail is docker compose's human-readable status string (e.g. "Up 3
	// hours", "Exited (1) 5 minutes ago"), or "container not found" when
	// the service has no container at all.
	Detail string
}

// TLSStatus reports the bundle's TLS certificate validity.
type TLSStatus struct {
	// NotAfter is the certificate's expiry time.
	NotAfter time.Time
	// Valid is true when the certificate is currently within its validity
	// window (not yet expired, not used before its NotBefore).
	Valid bool
	// ExpiringSoon is true when the certificate expires within
	// CertExpiryWarning.
	ExpiringSoon bool
}

// DiskStatus reports headroom on one filesystem path on the forge host.
type DiskStatus struct {
	Path           string
	TotalBytes     uint64
	UsedBytes      uint64
	AvailableBytes uint64
}

// Report is STAT-001's instance-health snapshot.
type Report struct {
	Services []ServiceStatus
	TLS      TLSStatus
	Disk     DiskStatus
}

// Runner executes one command against the forge host, returning its
// stdout. *orchestrate.Client satisfies it; status does not import
// orchestrate's Client type so it stays decoupled from the SSH transport
// and testable without one (the same posture as forge.Runner and
// state.Runner).
type Runner interface {
	Output(ctx context.Context, command string) ([]byte, error)
}

// DefaultServices are the Compose services checked when Options.Services is
// unset: the two services `up` deploys today (UP-002).
var DefaultServices = []string{forge.Service, caddy.Service}

// Options configures Check.
type Options struct {
	// Runner reaches the forge host; required.
	Runner Runner
	// Bundle is the loaded bundle being checked; required.
	Bundle *bundle.Bundle
	// RemoteDir is the directory on the host Converge deployed into,
	// matching the -remote-dir the operator gave `up`; required.
	RemoteDir string
	// Keystore resolves the bundle's TLS certificate; required.
	Keystore keystore.Driver
	// Services overrides which Compose services are checked. Defaults to
	// DefaultServices.
	Services []string
	// DiskPath overrides which filesystem path DiskStatus reports on.
	// Defaults to DefaultDiskPath.
	DiskPath string
	// Now overrides the clock TLSStatus is computed against. Defaults to
	// time.Now.
	Now func() time.Time
}

// Check builds a full Report: service run state via `docker compose ps`,
// TLS certificate validity via the keystore, and disk headroom via `df` —
// all read directly from the live host, so a Report always reflects the
// instance's current state rather than a cached one.
func Check(ctx context.Context, opts Options) (Report, error) {
	if opts.Runner == nil {
		return Report{}, errors.New("status: runner is required")
	}
	if opts.Bundle == nil {
		return Report{}, errors.New("status: bundle is required")
	}
	if opts.Keystore == nil {
		return Report{}, errors.New("status: keystore is required")
	}

	services, err := checkServices(ctx, opts)
	if err != nil {
		return Report{}, fmt.Errorf("status: services: %w", err)
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	tls, err := checkTLS(ctx, opts.Keystore, now())
	if err != nil {
		return Report{}, fmt.Errorf("status: tls: %w", err)
	}

	diskPath := strings.TrimSpace(opts.DiskPath)
	if diskPath == "" {
		diskPath = DefaultDiskPath
	}
	disk, err := checkDisk(ctx, opts.Runner, diskPath)
	if err != nil {
		return Report{}, fmt.Errorf("status: disk: %w", err)
	}

	return Report{Services: services, TLS: tls, Disk: disk}, nil
}

// composePSEntry is the subset of `docker compose ps --format json`'s
// per-container fields status needs.
type composePSEntry struct {
	Service string `json:"Service"`
	State   string `json:"State"`
	Status  string `json:"Status"`
}

func checkServices(ctx context.Context, opts Options) ([]ServiceStatus, error) {
	prefix, err := orchestrate.ComposeCommand(opts.RemoteDir, opts.Bundle)
	if err != nil {
		return nil, err
	}

	out, err := opts.Runner.Output(ctx, prefix+" docker compose ps --all --format json")
	if err != nil {
		return nil, fmt.Errorf("docker compose ps: %w", err)
	}

	byName, err := parseComposePS(out)
	if err != nil {
		return nil, err
	}

	names := opts.Services
	if len(names) == 0 {
		names = DefaultServices
	}

	result := make([]ServiceStatus, 0, len(names))
	for _, name := range names {
		entry, ok := byName[name]
		if !ok {
			result = append(result, ServiceStatus{Name: name, Up: false, Detail: "container not found"})
			continue
		}
		result = append(result, ServiceStatus{Name: name, Up: entry.State == "running", Detail: entry.Status})
	}
	return result, nil
}

// parseComposePS decodes `docker compose ps --format json` output, keyed by
// service name. Compose versions differ on whether this is a single JSON
// array or newline-delimited JSON objects, so parseComposePS tries the
// array shape first and falls back to NDJSON.
func parseComposePS(data []byte) (map[string]composePSEntry, error) {
	data = bytes.TrimSpace(data)
	byName := make(map[string]composePSEntry)
	if len(data) == 0 {
		return byName, nil
	}

	var entries []composePSEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		entries = nil
		for _, line := range bytes.Split(data, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var entry composePSEntry
			if err := json.Unmarshal(line, &entry); err != nil {
				return nil, fmt.Errorf("parse docker compose ps output: %w", err)
			}
			entries = append(entries, entry)
		}
	}

	for _, entry := range entries {
		byName[entry.Service] = entry
	}
	return byName, nil
}

func checkTLS(ctx context.Context, driver keystore.Driver, now time.Time) (TLSStatus, error) {
	secret, err := driver.Resolve(ctx, state.KeyTLSCertificate)
	if err != nil {
		return TLSStatus{}, fmt.Errorf("resolve certificate: %w", err)
	}

	block, _ := pem.Decode([]byte(secret.Reveal()))
	if block == nil {
		return TLSStatus{}, errors.New("certificate is not valid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return TLSStatus{}, fmt.Errorf("parse certificate: %w", err)
	}

	valid := now.After(cert.NotBefore) && now.Before(cert.NotAfter)
	expiringSoon := !now.Add(CertExpiryWarning).Before(cert.NotAfter)
	return TLSStatus{NotAfter: cert.NotAfter, Valid: valid, ExpiringSoon: expiringSoon}, nil
}

func checkDisk(ctx context.Context, runner Runner, path string) (DiskStatus, error) {
	out, err := runner.Output(ctx, "df -Pk "+shQuote(path))
	if err != nil {
		return DiskStatus{}, fmt.Errorf("df %s: %w", path, err)
	}
	return parseDF(out, path)
}

// parseDF parses POSIX `df -Pk`'s output: a header line followed by one
// data line, 1024-byte blocks in the second, third, and fourth
// whitespace-separated fields (Filesystem, 1024-blocks, Used, Available).
// -P forces this single-line, whitespace-separated layout across platforms
// (Linux and macOS both support it), so parseDF doesn't need per-OS cases.
func parseDF(out []byte, path string) (DiskStatus, error) {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) < 2 {
		return DiskStatus{}, fmt.Errorf("parse df output: expected a header and a data line, got %d line(s)", len(lines))
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return DiskStatus{}, fmt.Errorf("parse df output: unexpected format: %q", lines[len(lines)-1])
	}

	totalKB, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return DiskStatus{}, fmt.Errorf("parse df output: total blocks: %w", err)
	}
	usedKB, err := strconv.ParseUint(fields[2], 10, 64)
	if err != nil {
		return DiskStatus{}, fmt.Errorf("parse df output: used blocks: %w", err)
	}
	availKB, err := strconv.ParseUint(fields[3], 10, 64)
	if err != nil {
		return DiskStatus{}, fmt.Errorf("parse df output: available blocks: %w", err)
	}

	return DiskStatus{
		Path:           path,
		TotalBytes:     totalKB * 1024,
		UsedBytes:      usedKB * 1024,
		AvailableBytes: availKB * 1024,
	}, nil
}

// shQuote wraps s in single quotes for safe interpolation into a POSIX
// shell command, escaping any single quote s already contains.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
