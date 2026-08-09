// Package upgrade implements UPGR-001: upgrading a healthy instance in
// place — backup, bump the pinned Forgejo version, apply migrations,
// verify — by sequencing already-landed pieces rather than reimplementing
// any of them: status.Check (STAT-001) gates the run on instance health;
// backup.Backup (BKUP-001..006) captures a full, verified, encrypted
// snapshot of the pre-upgrade instance before anything changes;
// registry.Resolve (CORE-001, tech-spec.md "Bundle directory") pins the
// operator-named Forgejo image to a digest; and deploy.Up (UP-001..004)
// converges the host to that newly pinned image, which is what "applies
// migrations" means here — Forgejo runs its own schema migrations when it
// starts on a newer version (spec.md "Version pinning": "Schema migrations
// run during upgrades, never during restores"), and Upgrade's job is
// sequencing that safely, not reimplementing migration logic.
//
// Ordering is load-bearing: the pre-upgrade backup (step 2) must complete
// before the version is bumped (step 3), so the snapshot it produces still
// pins the pre-upgrade version — exactly what BuildOptions.ForgejoVersion
// captures from the bundle's own, still-unbumped Manifest.Images. Bumping
// first would silently embed the post-upgrade version into what's supposed
// to be a pre-upgrade snapshot, breaking both "every backup embeds the
// exact Forgejo version that wrote it" and "restore always runs that exact
// version" (spec.md "Version pinning") at once.
//
// That same ordering is what makes UPGR-002 reachable: once the backup has
// been written, every later failure path reports the snapshot it produced
// and the command that restores it, and nothing ever removes it. See
// recovery.go.
package upgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/registry"
	"github.com/evandelacruz/farrier/internal/core/status"
)

// Step identifiers Upgrade itself emits through the job's event stream
// (CORE-002), beyond the ones backup.Backup and deploy.Up relay through
// unchanged once each is called.
// StepVerify is deliberately distinct from backup.StepVerify ("verify",
// relayed onto the same job during the pre-upgrade backup): the two would
// otherwise collide on the shared event stream and each report as started
// or terminated twice.
const (
	StepCheckHealth = "check-health"
	StepBumpVersion = "bump-version"
	StepVerify      = "verify-upgrade"
)

// Host is everything Upgrade needs from a connected SSH session:
// deploy.Host (what deploy.Up itself requires) plus Target, which
// backup.BuildOptions needs to build the pre-upgrade backup's SSH-backed
// state exporters. *orchestrate.Client satisfies it in production.
type Host interface {
	deploy.Host
	Target() orchestrate.Target
}

// Resolver resolves an image reference to its digest-pinned form.
// Satisfied by registry.Resolver; declared here so Upgrade is testable
// without real network calls, the same shape initialize.Resolver takes.
type Resolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// Options configures Upgrade.
type Options struct {
	// BundleDir is the local directory Bundle was loaded from. Once the
	// new Forgejo image resolves, Upgrade saves the bumped manifest and
	// re-rendered Compose back here (CORE-001 bundle content), so later
	// commands — status, backup, a future up — see the upgraded pin.
	BundleDir string

	// RemoteDir is the directory on the host Upgrade deploys into.
	RemoteDir string

	// WorkDir is the local scratch directory the pre-upgrade backup
	// captures into. Upgrade creates it if it doesn't exist and removes it
	// — and everything under it — before returning, on both the success
	// and failure paths.
	WorkDir string

	// Bundle is the target bundle, loaded from BundleDir, still pinned to
	// the pre-upgrade Forgejo version when Upgrade is called.
	Bundle *bundle.Bundle

	// Destination is where the pre-upgrade backup is written (BKUP-005):
	// an S3-compatible URI or a filesystem path, the same shape
	// `backup -to` takes.
	Destination string

	// NewImage is the Forgejo image reference to upgrade to — a tag or an
	// exact digest. Resolver pins it to a digest the same way `init` pins
	// DefaultImageRefs.
	NewImage string

	// Identity is the bundle's age backup key: it encrypts the pre-upgrade
	// backup (BKUP-003).
	Identity *age.X25519Identity

	// Keystore is the bundle's keystore driver: it resolves the key
	// material the pre-upgrade backup captures and the certificate the
	// health checks validate.
	Keystore keystore.Driver

	// Blobs is the bundle's blob.Adapter: the pre-upgrade backup captures
	// from it (STATE-003).
	Blobs blob.Adapter

	// Host is the already-connected session to the forge host.
	Host Host

	// CertIssuer is passed through to deploy.Up unchanged; nil uses the
	// real ACME-backed issuer.
	CertIssuer deploy.CertIssuer

	// Resolver resolves NewImage to a digest; nil uses registry.Resolve.
	Resolver Resolver

	// DiskPath overrides which filesystem path the health checks report
	// disk headroom on. Defaults to status.DefaultDiskPath.
	DiskPath string
}

func (o Options) validate() error {
	if strings.TrimSpace(o.BundleDir) == "" {
		return errors.New("upgrade: bundle directory is required")
	}
	if strings.TrimSpace(o.RemoteDir) == "" {
		return errors.New("upgrade: remote directory is required")
	}
	if strings.TrimSpace(o.WorkDir) == "" {
		return errors.New("upgrade: work directory is required")
	}
	if o.Bundle == nil {
		return errors.New("upgrade: bundle is required")
	}
	if strings.TrimSpace(o.Destination) == "" {
		return errors.New("upgrade: backup destination is required")
	}
	if strings.TrimSpace(o.NewImage) == "" {
		return errors.New("upgrade: new forgejo image is required")
	}
	if o.Identity == nil {
		return errors.New("upgrade: age identity is required")
	}
	if o.Keystore == nil {
		return errors.New("upgrade: keystore driver is required")
	}
	if o.Blobs == nil {
		return errors.New("upgrade: blob adapter is required")
	}
	if o.Host == nil {
		return errors.New("upgrade: host is required")
	}
	if inside, err := destinationInsideWorkDir(o.Destination, o.WorkDir); err != nil {
		return err
	} else if inside {
		return fmt.Errorf("upgrade: backup destination %s is inside the work directory %s, which is deleted when upgrade returns; name a destination outside it", o.Destination, o.WorkDir)
	}
	return nil
}

// destinationInsideWorkDir reports whether destination is a filesystem
// path at or beneath workDir. Upgrade deletes workDir and everything under
// it on every exit path, so such a destination would take the pre-upgrade
// snapshot with it on exactly the failure UPGR-002 exists to survive —
// refusing up front is the only place that can be caught before the backup
// is taken.
//
// A destination is a filesystem path unless it is an s3:// URI, matching
// how backup.OpenDestination itself decides (BKUP-005): anything else,
// including a string that merely looks scheme-ish, is passed to
// blob.NewLocal as a path.
func destinationInsideWorkDir(destination, workDir string) (bool, error) {
	if strings.HasPrefix(destination, "s3://") {
		return false, nil
	}
	absDest, err := filepath.Abs(destination)
	if err != nil {
		return false, fmt.Errorf("upgrade: resolve backup destination %s: %w", destination, err)
	}
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return false, fmt.Errorf("upgrade: resolve work directory %s: %w", workDir, err)
	}
	rel, err := filepath.Rel(absWork, absDest)
	if err != nil {
		// Different volumes on Windows: not nested, by construction.
		return false, nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, nil
	}
	return true, nil
}

// Upgrade runs UPGR-001 end to end as one CORE-002 job: refuse unless the
// instance is healthy (status.Check), capture a verified, encrypted backup
// of the pre-upgrade instance (backup.Backup), resolve and pin the new
// Forgejo image into the bundle manifest, converge the host to it
// (deploy.Up — Forgejo migrates its own schema on that restart), and
// verify the upgraded instance is healthy again.
//
// Upgrade owns job's terminal event, calling job.Succeeded or job.Failed
// exactly once, after every step below has run or the first one fails.
//
// A failure past the pre-upgrade backup carries the path back to the
// pre-upgrade version (UPGR-002, recovery.go) on both the returned error
// and a StepRecoveryPath event, so the operator sees it whichever frontend
// they ran the upgrade from.
func Upgrade(ctx context.Context, job *events.Job, opts Options) error {
	version, recovery, err := upgrade(ctx, job, opts)
	if err != nil {
		if recovery != nil {
			job.Emit(StepRecoveryPath, events.StateSucceeded, recovery.Detail())
			err = fmt.Errorf("%w\n%s", err, recovery.Detail())
		}
		job.Failed(err.Error())
		return err
	}
	job.Succeeded(fmt.Sprintf("upgraded to forgejo %s", version))
	return nil
}

// upgrade runs the sequence and returns, alongside any failure, the
// Recovery describing the path back — non-nil from the moment the
// pre-upgrade backup lands, nil before it, because before it there is no
// snapshot from this run to point at and nothing has changed yet either.
func upgrade(ctx context.Context, job *events.Job, opts Options) (string, *Recovery, error) {
	if err := opts.validate(); err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(opts.WorkDir, 0o700); err != nil {
		return "", nil, fmt.Errorf("upgrade: create work directory: %w", err)
	}
	// Removes local scratch only. The pre-upgrade snapshot lives at
	// opts.Destination, which validate has already refused to let nest
	// inside WorkDir (UPGR-002).
	defer os.RemoveAll(opts.WorkDir)

	if err := checkHealthy(ctx, job, opts, opts.Bundle, StepCheckHealth, "refusing to upgrade"); err != nil {
		return "", nil, err
	}

	snapshotKey, err := runBackup(ctx, job, opts)
	if err != nil {
		return "", nil, err
	}
	recovery := &Recovery{
		Destination:    opts.Destination,
		SnapshotKey:    snapshotKey,
		ForgejoVersion: opts.Bundle.Manifest.Images[forge.Service],
		Target:         opts.Host.Target().String(),
		BundleDir:      opts.BundleDir,
	}

	bumped, err := bumpVersion(ctx, job, opts)
	if err != nil {
		return "", recovery, err
	}

	if err := runDeploy(ctx, job, bumped, opts); err != nil {
		return "", recovery, err
	}

	if err := checkHealthy(ctx, job, opts, bumped, StepVerify, "upgraded instance failed verification"); err != nil {
		return "", recovery, err
	}

	return bumped.Manifest.Images[forge.Service], nil, nil
}

// checkHealthy runs status.Check against b and refuses — naming every
// specific problem found, the same "name the reason" posture RSTR-003 and
// BKUP-004 already take — unless every service is up, TLS is valid, and
// disk headroom remains. It is used both as UPGR-001's pre-upgrade gate
// (StepCheckHealth) and as its post-upgrade verify step (StepVerify): the
// same instance-health notion status.Check already reports (STAT-001), run
// twice against two different bundles (the pre- and post-bump pin).
func checkHealthy(ctx context.Context, job *events.Job, opts Options, b *bundle.Bundle, step, refusalPrefix string) error {
	job.Started(step, "checking instance health")
	report, err := status.Check(ctx, status.Options{
		Runner:    opts.Host,
		Bundle:    b,
		RemoteDir: opts.RemoteDir,
		Keystore:  opts.Keystore,
		DiskPath:  opts.DiskPath,
	})
	if err != nil {
		err = fmt.Errorf("upgrade: check instance health: %w", err)
		job.Emit(step, events.StateFailed, err.Error())
		return err
	}
	if problems := unhealthy(report); len(problems) > 0 {
		err := fmt.Errorf("upgrade: %s: %s", refusalPrefix, strings.Join(problems, "; "))
		job.Emit(step, events.StateFailed, err.Error())
		return err
	}
	job.Emit(step, events.StateSucceeded, "instance is healthy")
	return nil
}

// unhealthy names every specific way report falls short of healthy: any
// checked service not up, an invalid (expired or not-yet-valid) TLS
// certificate, or no disk headroom left at all. Disk headroom is checked
// as a bug backstop — completely exhausted — not a percentage policy: no
// numeric "safe" threshold is settled anywhere in docs/, so this only
// refuses the unambiguous case rather than inventing one.
func unhealthy(r status.Report) []string {
	var problems []string
	for _, s := range r.Services {
		if !s.Up {
			problems = append(problems, fmt.Sprintf("service %s is down: %s", s.Name, s.Detail))
		}
	}
	if !r.TLS.Valid {
		problems = append(problems, fmt.Sprintf("tls certificate is not valid (expires %s)", r.TLS.NotAfter.Format(time.RFC3339)))
	}
	if r.Disk.AvailableBytes == 0 {
		problems = append(problems, fmt.Sprintf("no disk headroom left on %s", r.Disk.Path))
	}
	return problems
}

// runBackup captures a full, verified, encrypted snapshot of opts.Bundle —
// still pinned to the pre-upgrade Forgejo version — through backup.Backup,
// relaying every step event it emits onto job as it happens, the same
// relay pattern promote.restoreOnto and backup.runCapture already use: a
// shared job here would close job's stream the moment Backup finishes and
// panic on every Emit call Upgrade made afterward.
// It returns the destination key the snapshot was written under, which is
// what a later failure points the operator back at (UPGR-002).
func runBackup(ctx context.Context, job *events.Job, opts Options) (string, error) {
	backupJob := events.NewJob()
	stream, cancel := backupJob.Subscribe()
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

	backupOpts := backup.BuildOptions(opts.Host, opts.Bundle, opts.RemoteDir, filepath.Join(opts.WorkDir, "backup"), opts.Destination, opts.Identity, opts.Blobs, opts.Keystore)
	key, err := backup.Backup(ctx, backupJob, backupOpts)
	<-relayed
	if err != nil {
		return "", fmt.Errorf("upgrade: pre-upgrade backup: %w", err)
	}
	return key, nil
}

// bumpVersion resolves opts.NewImage to a digest and returns a copy of
// opts.Bundle with the forge image pinned to it, saved back to
// opts.BundleDir so the bump survives past this run (CORE-001 bundle
// content). opts.Bundle itself is left untouched.
func bumpVersion(ctx context.Context, job *events.Job, opts Options) (*bundle.Bundle, error) {
	job.Started(StepBumpVersion, fmt.Sprintf("resolving %s", opts.NewImage))

	resolved, err := resolverOrDefault(opts.Resolver).Resolve(ctx, opts.NewImage)
	if err != nil {
		err = fmt.Errorf("upgrade: resolve forgejo image: %w", err)
		job.Emit(StepBumpVersion, events.StateFailed, err.Error())
		return nil, err
	}

	bumped, err := bumpedBundle(opts.Bundle, resolved)
	if err != nil {
		job.Emit(StepBumpVersion, events.StateFailed, err.Error())
		return nil, err
	}

	if err := bumped.Save(opts.BundleDir); err != nil {
		err = fmt.Errorf("upgrade: save bumped manifest: %w", err)
		job.Emit(StepBumpVersion, events.StateFailed, err.Error())
		return nil, err
	}

	job.Emit(StepBumpVersion, events.StateSucceeded, fmt.Sprintf("pinned forgejo to %s", resolved))
	return bumped, nil
}

// bumpedBundle returns a copy of b with the forge image pinned to
// resolvedImage, Compose re-rendered from that override the same way
// restore.pinnedBundle builds the version it deploys during a restore:
// deploy.Up ships b.Compose to the host as-is, so the manifest alone isn't
// enough — Compose has to carry the new image too.
//
// b itself is left untouched: Images is copied before the override, so the
// caller's own bundle (and the map backing its Manifest.Images) is never
// mutated out from under it.
func bumpedBundle(b *bundle.Bundle, resolvedImage string) (*bundle.Bundle, error) {
	manifest := b.Manifest
	images := make(map[string]string, len(manifest.Images))
	for component, image := range manifest.Images {
		images[component] = image
	}
	images[forge.Service] = resolvedImage
	manifest.Images = images

	compose, err := orchestrate.Render(&manifest)
	if err != nil {
		return nil, fmt.Errorf("upgrade: render compose pinned to forgejo %s: %w", resolvedImage, err)
	}
	return &bundle.Bundle{Manifest: manifest, Compose: compose}, nil
}

// runDeploy converges opts.Host to bumped's definition through deploy.Up —
// the container recreation that makes Forgejo start on the new image and
// run its own schema migrations (UPGR-003) — relaying every step event it
// emits onto job as it happens, the same relay reason runBackup documents.
func runDeploy(ctx context.Context, job *events.Job, bumped *bundle.Bundle, opts Options) error {
	deployJob := events.NewJob()
	stream, cancel := deployJob.Subscribe()
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

	err := deploy.Up(ctx, deployJob, opts.Host, bumped, deploy.Options{
		RemoteDir:  opts.RemoteDir,
		CertIssuer: opts.CertIssuer,
	})
	<-relayed
	if err != nil {
		return fmt.Errorf("upgrade: apply migrations: %w", err)
	}
	return nil
}

func resolverOrDefault(r Resolver) Resolver {
	if r != nil {
		return r
	}
	return registry.Resolver{}
}
