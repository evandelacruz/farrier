// Package drill implements the rehearsal command `drill` (DRIL-001):
// restore the most recent backup onto a scratch target, boot the full
// stack there, and report success or the specific step that failed.
//
// Drill sequences already-landed pieces rather than reimplementing any of
// them. backup.SnapshotAge resolves the newest snapshot at the destination
// — the same "empty key means newest" resolution restore and promote apply
// — and reports its age. restore.Restore (RSTR-001..004) then fetches,
// decrypts, and verifies that snapshot, refuses on a failed verification
// naming the defect, installs the snapshot's original identity, and ends by
// running deploy.Up (UP-001..004), which is what "boot the full stack"
// means here: the same converge, readiness wait, and admin bootstrap a real
// deployment runs, against the state the restore just placed.
//
// # What drill adds on top of restore
//
// Naming the failing step. A restore reports that it failed; a rehearsal is
// only useful if it says *where*. Drill runs restore against a private job
// and relays its CORE-002 step stream onto its own (the same relay promote
// and upgrade use, for the same reason: restore ends whatever job it is
// given), recording the first step that reported StateFailed. That step
// name and its detail are what Report and the returned *Failure carry, so
// "the drill failed at verify-snapshot" and "the drill failed at
// wait-forge" are distinguishable without reading a log.
//
// # Two things drill deliberately does not do
//
// Drill never touches DNS. Flipping the bundle's record at a new host is
// promote's job (FAIL-004); a drill that repointed the domain at a scratch
// host would take production down — the opposite of a rehearsal. Nothing
// in this package imports internal/core/dns, and a test enforces that.
//
// Drill never reconciles CI. restore.Options.ReconcileCI stays false, so
// the orphaned `running` rows in the restored database are left exactly as
// the snapshot recorded them. promote sets it because the promoted
// instance is becoming production and those jobs must re-dispatch
// (FAIL-003); a drill instance is a rehearsal carrying production's
// identity, and resetting those rows to `queued` would arm it to run
// production's CI for real.
//
// # Quarantine (DRIL-002)
//
// A drill instance is a full restore of production: the same database, the
// same key material, the same domain in its config, the same webhook rows
// and push mirrors. Everything that makes it a faithful rehearsal also
// makes it capable of acting as production. Quarantine is what keeps it
// from doing so, and it holds for every drilled instance — not only while
// a smoke job is running, and not as anything a caller can turn off.
//
// A drilled instance can reach the snapshot it restored and nothing
// outside its host; the operator can reach it through an SSH tunnel to
// that host and nobody else can reach it at all. Three properties, in
// three places:
//
//   - Outbound webhooks, email, and mirrors are disabled in the rendered
//     app.ini (forge.AppINIOptions.Quarantine), reached from here through
//     restore.Options.Quarantine, which Drill sets unconditionally. It is a
//     config override rather than an edit to the restored database, so the
//     state under test stays exactly what the snapshot held.
//   - DNS is untouched, per the section above: the bundle's record keeps
//     pointing at production for the whole drill, so no client is ever
//     directed at the scratch target.
//   - Caddy's HTTPS port is published on the scratch host's loopback
//     interface rather than on every interface
//     (orchestrate.WithLoopbackPorts, via deploy.Options.Quarantine), so
//     the only route in is an SSH tunnel terminating on that host — the
//     same SSH access Farrier already needs to run the drill at all.
//
// What quarantine does not do is make the scratch target itself safe to
// share: anything with a shell on that host can reach the instance over
// loopback, exactly as it could reach any other container there. The
// scratch target is Farrier's to converge and the operator's to choose
// (spec.md "Rehearsal"), and it should be a host the operator would be
// willing to hand production's state to, because for the length of the
// drill that is what it holds.
package drill

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/restore"
)

// StepResolveSnapshot identifies the step Drill itself emits onto a job's
// event stream before restore.Restore's own steps relay through: resolving
// which snapshot at the destination is the most recent one (DRIL-001's
// "the most recent backup").
const StepResolveSnapshot = "resolve-snapshot"

// Host is everything Drill needs from a connected SSH session to the
// scratch target — exactly restore.Host, restore.Restore's own requirement.
type Host = restore.Host

// Options configures Drill. It is restore.Options minus the two things a
// rehearsal must not choose: there is no SnapshotKey (DRIL-001 drills the
// most recent backup, full stop) and no DNS anything (drill never touches
// DNS).
type Options struct {
	// RemoteDir is the directory on the scratch target Drill deploys into
	// — see restore.Options.RemoteDir.
	RemoteDir string

	// WorkDir is the local scratch directory the snapshot is fetched and
	// decrypted under — see restore.Options.WorkDir, which removes it on
	// every exit path.
	WorkDir string

	// Bundle is the bundle being rehearsed: its manifest and rendered
	// Compose are what the scratch target converges to. It is production's
	// own bundle, which is why the drilled instance carries production's
	// identity (spec.md "Rehearsal") and why DRIL-002 exists.
	Bundle *bundle.Bundle

	// Source is the backup destination (backup.OpenDestination) whose most
	// recent snapshot is drilled.
	Source blob.Adapter

	// Identity is the bundle's age backup key: it decrypts the fetched
	// snapshot.
	Identity *age.X25519Identity

	// Keystore is the scratch target's keystore driver the snapshot's key
	// material is installed into; it must implement keystore.Writer, the
	// same requirement restore.Options.Keystore places on it.
	Keystore keystore.Driver

	// Blobs is the scratch target's blob.Adapter blobs are restored into.
	Blobs blob.Adapter

	// Host is the already-connected session to the scratch target.
	Host Host

	// CertIssuer is passed through to restore.Restore (and, through it,
	// deploy.Up) unchanged; nil uses the real ACME-backed issuer.
	CertIssuer deploy.CertIssuer

	// Now overrides the clock the drilled snapshot's age is measured
	// against. Zero uses time.Now.
	Now time.Time
}

func (o Options) validate() error {
	if strings.TrimSpace(o.WorkDir) == "" {
		return errors.New("drill: work directory is required")
	}
	if strings.TrimSpace(o.RemoteDir) == "" {
		return errors.New("drill: remote directory is required")
	}
	if o.Bundle == nil {
		return errors.New("drill: bundle is required")
	}
	if o.Source == nil {
		return errors.New("drill: snapshot source is required")
	}
	if o.Identity == nil {
		return errors.New("drill: age identity is required")
	}
	if o.Keystore == nil {
		return errors.New("drill: keystore driver is required")
	}
	if o.Blobs == nil {
		return errors.New("drill: blob adapter is required")
	}
	if o.Host == nil {
		return errors.New("drill: host is required")
	}
	return nil
}

func (o Options) now() time.Time {
	if o.Now.IsZero() {
		return time.Now()
	}
	return o.Now
}

// Failure is DRIL-001's "the specific failing step": the step of the drill
// that failed, and the detail that step reported. Step is the CORE-002 step
// identifier — StepResolveSnapshot, or one of restore.Restore's and
// deploy.Up's own (restore.StepVerify, deploy.StepConverge, ...) — so a
// caller can act on it without parsing prose.
//
// Step is empty only when the drill failed before any step reported an
// outcome, which today means Options failed validation.
type Failure struct {
	Step   string
	Detail string
}

func (f *Failure) Error() string {
	if f.Step == "" {
		return fmt.Sprintf("drill: failed before any step reported an outcome: %s", f.Detail)
	}
	return fmt.Sprintf("drill: failed at step %q: %s", f.Step, f.Detail)
}

// Report is what a drill produces: which snapshot was drilled, how old it
// was, and either success or the specific failing step. It is the same
// value on both frontends — `drill` prints it, and a dashboard's drill
// results panel (UI-002) renders it from the same job.
type Report struct {
	// SnapshotKey is the destination key of the snapshot that was drilled:
	// the most recent one at Options.Source, resolved once before the
	// restore starts.
	SnapshotKey string

	// SnapshotAge is how old that snapshot was when the drill resolved it.
	SnapshotAge time.Duration

	// Failure is nil on success, and otherwise names the step that failed.
	Failure *Failure
}

// Succeeded reports whether the drill completed every step.
func (r Report) Succeeded() bool { return r.Failure == nil }

// Drill runs DRIL-001 as one CORE-002 job: resolve the most recent snapshot
// at opts.Source, restore it onto opts.Host, and boot the full stack there
// (restore.Restore, which ends in deploy.Up). It returns a Report naming
// the snapshot drilled and, on failure, the specific step that failed.
//
// The returned error is always a *Failure carrying the same step, so a
// caller that wants the step rather than the message can errors.As for it
// instead of reading Report.
//
// Drill owns job's terminal event, calling job.Succeeded or job.Failed
// exactly once, after every step below has run or the first one fails.
func Drill(ctx context.Context, job *events.Job, opts Options) (Report, error) {
	report := drill(ctx, job, opts)
	if report.Failure != nil {
		job.Failed(report.Failure.Error())
		return report, report.Failure
	}
	job.Succeeded(fmt.Sprintf(
		"drill succeeded: snapshot %s (captured %s ago) restored to the scratch target and the full stack booted",
		report.SnapshotKey, report.SnapshotAge.Round(time.Second),
	))
	return report, nil
}

func drill(ctx context.Context, job *events.Job, opts Options) Report {
	if err := opts.validate(); err != nil {
		return Report{Failure: &Failure{Detail: err.Error()}}
	}

	key, snapshotAge, err := resolveSnapshot(ctx, job, opts)
	if err != nil {
		return Report{Failure: &Failure{Step: StepResolveSnapshot, Detail: err.Error()}}
	}
	report := Report{SnapshotKey: key, SnapshotAge: snapshotAge}

	if step, err := restoreOnto(ctx, job, opts, key); err != nil {
		report.Failure = &Failure{Step: step, Detail: err.Error()}
	}
	return report
}

// resolveSnapshot resolves the most recent snapshot at opts.Source and its
// age. Drill resolves it here, once, and hands the resolved key to
// restore.Restore rather than letting Restore re-run its own "empty means
// newest" resolution — the same reason FAIL-002's confirmPromote does:
// re-resolving later could pick up a snapshot written in between and drill
// a different one than the report names.
func resolveSnapshot(ctx context.Context, job *events.Job, opts Options) (string, time.Duration, error) {
	job.Started(StepResolveSnapshot, "resolving the most recent snapshot")

	key, snapshotAge, err := backup.SnapshotAge(ctx, opts.Source, "", opts.now())
	if err != nil {
		err = fmt.Errorf("drill: resolve most recent snapshot: %w", err)
		job.Emit(StepResolveSnapshot, events.StateFailed, err.Error())
		return "", 0, err
	}

	job.Emit(StepResolveSnapshot, events.StateSucceeded, fmt.Sprintf(
		"drilling snapshot %s, captured %s ago", key, snapshotAge.Round(time.Second),
	))
	return key, snapshotAge, nil
}

// restoreOnto runs restore.Restore against a private job, relaying every
// step event it emits onto job as it happens — the same relay pattern
// promote.restoreOnto and upgrade.runDeploy use, needed for the same
// reason: restore.Restore ends whatever job it's given, so a shared job
// here would close job's stream before Drill could emit its own terminal
// event.
//
// The relay is also where DRIL-001's "report the specific failing step"
// comes from: it records the first step that reports StateFailed, which is
// the step the whole drill failed at — every later step is skipped, since
// restore returns at the first failure.
func restoreOnto(ctx context.Context, job *events.Job, opts Options, snapshotKey string) (string, error) {
	restoreJob := events.NewJob()
	stream, cancel := restoreJob.Subscribe()
	defer cancel()

	var failedStep string
	relayed := make(chan struct{})
	go func() {
		defer close(relayed)
		for ev := range stream {
			if ev.Step == "" {
				continue
			}
			if ev.State == events.StateFailed && failedStep == "" {
				failedStep = ev.Step
			}
			job.Emit(ev.Step, ev.State, ev.Detail)
		}
	}()

	err := restore.Restore(ctx, restoreJob, restore.Options{
		RemoteDir:   opts.RemoteDir,
		WorkDir:     opts.WorkDir,
		Bundle:      opts.Bundle,
		Source:      opts.Source,
		SnapshotKey: snapshotKey,
		Identity:    opts.Identity,
		Keystore:    opts.Keystore,
		Blobs:       opts.Blobs,
		Host:        opts.Host,
		CertIssuer:  opts.CertIssuer,
		// Deliberately false — see the package doc comment: a drill is a
		// rehearsal, not a takeover, and re-queueing production's orphaned
		// CI jobs would arm the drill instance to run them.
		ReconcileCI: false,
		// Unconditionally true (DRIL-002). Quarantine is a property of
		// every drilled instance, not of whatever happens to be running on
		// one, so there is no Options field a caller could clear: a drill
		// that could reach the outside world is not a drill.
		Quarantine: true,
	})
	// Reading failedStep after the relay goroutine has closed `relayed`
	// synchronizes with every write it made.
	<-relayed
	return failedStep, err
}
