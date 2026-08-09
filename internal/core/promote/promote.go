// Package promote implements FAIL-001: failover as one command. Promote
// sequences already-landed pieces rather than reimplementing them —
// restore.Restore (RSTR-001..003) to restore the latest snapshot onto a
// fresh host, verify it, and start services (restore.Restore itself ends
// with deploy.Up); forge.ReconcileCI (FORGE-004), via
// restore.Options.ReconcileCI, to reset the snapshot's orphaned `running`
// CI jobs to `queued` before that database is placed on the host
// (FAIL-003's automatic re-dispatch depends on this having already run);
// and internal/core/dns's driver interface to flip the bundle's domain at
// the new host, or print the exact record change when no DNS driver is
// configured (DNS-003, FAIL-004).
//
// spec.md "Failover" describes promotion as a cold-standby model: a
// snapshot in the backup target plus a fresh host, restored and pointed at
// on demand, with an accepted data-loss window and a few minutes of
// downtime rather than an automatic health-triggered failover.
package promote

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"filippo.io/age"

	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/dns"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/restore"
)

// StepDNSFlip identifies the DNS-flip step Promote itself emits onto a
// job's event stream, beyond the ones restore.Restore relays through
// unchanged once it's called (Promote's own last step).
const StepDNSFlip = "dns-flip"

// Host is everything Promote needs from a connected SSH session — exactly
// restore.Host, restore.Restore's own requirement.
type Host = restore.Host

// Options configures Promote: restore.Restore's own inputs, plus the DNS
// driver and record value the failover flip applies.
type Options struct {
	// RemoteDir is the directory on the host Promote deploys into — see
	// restore.Options.RemoteDir.
	RemoteDir string

	// WorkDir is the local scratch directory Promote fetches and decrypts
	// the snapshot under — see restore.Options.WorkDir.
	WorkDir string

	// Bundle is the target bundle: its manifest and rendered Compose are
	// what services converge to, and its domain is the DNS record flipped
	// by default (DNSRecord).
	Bundle *bundle.Bundle

	// Source is the backup destination (backup.OpenDestination) the latest
	// snapshot is restored from.
	Source blob.Adapter

	// SnapshotKey names which snapshot to restore. Empty resolves to the
	// newest object in Source (spec.md "Standby model: cold").
	SnapshotKey string

	// Identity is the bundle's age backup key: it decrypts the fetched
	// snapshot.
	Identity *age.X25519Identity

	// Keystore is the target keystore driver key material is installed
	// into; it must implement keystore.Writer, the same requirement
	// restore.Options.Keystore places on it.
	Keystore keystore.Driver

	// Blobs is the target blob.Adapter blobs are restored into.
	Blobs blob.Adapter

	// Host is the already-connected session to the standby host.
	Host Host

	// CertIssuer is passed through to restore.Restore (and, through it,
	// deploy.Up) unchanged; nil uses the real ACME-backed issuer.
	CertIssuer deploy.CertIssuer

	// DNS is the driver the DNS flip applies through — dns.NewPrint(job)
	// when the bundle has no DNS driver configured (DNS-003), or a real
	// driver built from it (ResolveDNSDriver builds either from a bundle's
	// DriverConfig.DNS). Required: Promote never resolves this itself, the
	// same "caller resolves, core executes" split restore.Options.Keystore
	// and .Blobs already draw.
	DNS dns.Driver

	// DNSRecord is the record the flip upserts. Empty defaults to
	// Bundle.Manifest.Domain — the bundle's own domain, the record every
	// other DNS-changing operation in this codebase means by "the bundle's
	// record" (dns.go's doc comment).
	DNSRecord string

	// DNSValue is the address (an IP or a hostname) DNSRecord is pointed
	// at — the standby host's own public address. Required: this is the
	// operator's topology, the same reason `up` never manages DNS itself
	// (tech-spec.md "Deployment").
	DNSValue string
}

func (o Options) validate() error {
	if strings.TrimSpace(o.WorkDir) == "" {
		return errors.New("promote: work directory is required")
	}
	if strings.TrimSpace(o.RemoteDir) == "" {
		return errors.New("promote: remote directory is required")
	}
	if o.Bundle == nil {
		return errors.New("promote: bundle is required")
	}
	if o.Source == nil {
		return errors.New("promote: snapshot source is required")
	}
	if o.Identity == nil {
		return errors.New("promote: age identity is required")
	}
	if o.Keystore == nil {
		return errors.New("promote: keystore driver is required")
	}
	if o.Blobs == nil {
		return errors.New("promote: blob adapter is required")
	}
	if o.Host == nil {
		return errors.New("promote: host is required")
	}
	if o.DNS == nil {
		return errors.New("promote: dns driver is required")
	}
	if strings.TrimSpace(o.DNSValue) == "" {
		return errors.New("promote: dns value is required")
	}
	return nil
}

// dnsRecord returns opts.DNSRecord, defaulting to the bundle's own domain.
func (o Options) dnsRecord() string {
	if strings.TrimSpace(o.DNSRecord) != "" {
		return o.DNSRecord
	}
	return o.Bundle.Manifest.Domain
}

// Promote runs FAIL-001 end to end as one CORE-002 job: restore the latest
// (or named) snapshot onto opts.Host, verifying it and starting services
// (restore.Restore, with ReconcileCI set so orphaned CI jobs are already
// queued by the time services start and Forgejo's own scheduler dispatches
// them — FAIL-003), then flip opts.DNSRecord to opts.DNSValue through
// opts.DNS (FAIL-004).
//
// Promote owns job's terminal event, calling job.Succeeded or job.Failed
// exactly once, after every step below has run or the first one fails.
func Promote(ctx context.Context, job *events.Job, opts Options) error {
	if err := promote(ctx, job, opts); err != nil {
		job.Failed(err.Error())
		return err
	}
	job.Succeeded(fmt.Sprintf("failed over to a fresh host and flipped %s to %s", opts.dnsRecord(), opts.DNSValue))
	return nil
}

func promote(ctx context.Context, job *events.Job, opts Options) error {
	if err := opts.validate(); err != nil {
		return err
	}

	if err := restoreOnto(ctx, job, opts); err != nil {
		return err
	}

	return flipDNS(ctx, job, opts)
}

// restoreOnto runs restore.Restore against a private job, relaying every
// step event it emits onto job as it happens — the same relay pattern
// restore.go's own runDeploy uses for deploy.Up, needed for the same
// reason: restore.Restore ends whatever job it's given, so a shared job
// here would close job's stream the moment Restore finishes.
func restoreOnto(ctx context.Context, job *events.Job, opts Options) error {
	restoreJob := events.NewJob()
	stream, cancel := restoreJob.Subscribe()
	defer cancel()

	relayed := make(chan struct{})
	go func() {
		defer close(relayed)
		for ev := range stream {
			if ev.Step == "" {
				continue
			}
			job.Emit(ev.Step, ev.State, ev.Detail)
		}
	}()

	err := restore.Restore(ctx, restoreJob, restore.Options{
		RemoteDir:   opts.RemoteDir,
		WorkDir:     opts.WorkDir,
		Bundle:      opts.Bundle,
		Source:      opts.Source,
		SnapshotKey: opts.SnapshotKey,
		Identity:    opts.Identity,
		Keystore:    opts.Keystore,
		Blobs:       opts.Blobs,
		Host:        opts.Host,
		CertIssuer:  opts.CertIssuer,
		ReconcileCI: true,
	})
	<-relayed
	return err
}

// flipDNS upserts opts.dnsRecord() to opts.DNSValue through opts.DNS
// (spec.md "Failover" step 4, FAIL-004), using dns.SetBundleRecord so the
// flip carries the same 60-second TTL every other bundle record does
// (DNS-004). It reports through job even for drivers (Cloudflare, RFC
// 2136) that emit no event of their own — dns.PrintDriver is the only
// Driver that reports through job itself — so the DNS step always has
// CORE-002 visibility regardless of which driver is configured.
func flipDNS(ctx context.Context, job *events.Job, opts Options) error {
	record := opts.dnsRecord()
	job.Started(StepDNSFlip, fmt.Sprintf("updating dns: %s -> %s", record, opts.DNSValue))

	if err := dns.SetBundleRecord(ctx, opts.DNS, record, opts.DNSValue); err != nil {
		err = fmt.Errorf("promote: update dns: %w", err)
		job.Emit(StepDNSFlip, events.StateFailed, err.Error())
		return err
	}

	job.Emit(StepDNSFlip, events.StateSucceeded, fmt.Sprintf("dns record %s now points to %s", record, opts.DNSValue))
	return nil
}
