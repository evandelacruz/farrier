// Package restore implements RSTR-001: rebuilding a complete, verified
// working instance on a fresh host from a snapshot and a bundle.
//
// Restore fetches a snapshot from a backup destination (backup.OpenDestination,
// BKUP-005), decrypts it and verifies it with exactly the code path backup
// verifies a snapshot with at creation time (backup.DecryptArchive,
// backup.Verify — BKUP-004, tech-spec.md "Snapshot format": "Verification
// at creation and at restore runs the same code path") — refusing to
// proceed, naming every defect found, if verification fails. Once verified,
// it installs the snapshot's key material into the target keystore
// (mirroring initialize.Run's write path, INIT-003), places the snapshot's
// git data and database directly onto the host directories UP-004 pins
// forge state to, restores blobs through the target's blob.Adapter
// (STATE-003), and finally runs the same deploy.Up sequence `up` uses
// (UP-001..004) to bring the stack up against the state it just placed —
// deploy.Up's own admin bootstrap step (forge.Bootstrap) sees the restored
// database already has an admin account and treats that as done, exactly
// as it does on a repeat `up` (UP-003), rather than minting a new one.
//
// Restore also runs deploy.Up against the exact Forgejo image the snapshot
// was captured from (RSTR-002, spec.md "Version pinning"), never whatever
// image the target bundle's own farrier.yaml currently pins — the snapshot
// manifest's ForgejoVersion overrides the target bundle's forge image, and
// Compose is re-rendered from that override the same way orchestrate.Render
// builds it at init time, before deploy.Up ever runs.
//
// This does not install the SSH host key onto the running Forgejo service
// so an existing known_hosts entry keeps working (RSTR-004) —
// restore.Restore installs it into the target keystore like every other
// captured key, ready for RSTR-004 to wire up — and while reusing
// backup.Verify already gives restore the specific, named refusal RSTR-003
// requires, RSTR-003 itself is a separate ID with its own acceptance bar.
package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// Step identifiers Restore itself emits through the job's event stream,
// beyond the ones deploy.Up relays through unchanged once it's called
// (Restore's own last step).
const (
	StepFetch        = "fetch-snapshot"
	StepDecrypt      = "decrypt-snapshot"
	StepVerify       = "verify-snapshot"
	StepInstallKeys  = "install-keys"
	StepPlaceState   = "place-state"
	StepRestoreBlobs = "restore-blobs"
)

// Host is everything Restore needs from a connected SSH session: deploy.Up's
// own requirement (deploy.Host — orchestrate.Transport plus Run and
// CheckHost), plus RunStdin, which Restore uses to stream a decrypted
// snapshot's git archives and database file directly onto the host without
// holding either in memory. *orchestrate.Client satisfies it in production.
type Host interface {
	deploy.Host
	RunStdin(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error
}

// Options configures Restore: everything it needs beyond the connected host
// and the target bundle.
type Options struct {
	// RemoteDir is the directory on the host Restore deploys into — the
	// same meaning as deploy.Options.RemoteDir, and where GitStatePath and
	// GiteaStatePath place the snapshot's git data and database before
	// deploy.Up ever starts the forgejo container that reads them.
	RemoteDir string

	// WorkDir is the local scratch directory Restore fetches the encrypted
	// snapshot into and decrypts it under. Restore creates it if it
	// doesn't exist and removes it — and everything under it — before
	// returning, on both the success and failure paths: a plaintext
	// snapshot is never left on disk once Restore returns.
	WorkDir string

	// Bundle is the target bundle: its manifest and rendered Compose are
	// what deploy.Up converges the host to once state is in place.
	Bundle *bundle.Bundle

	// Source is the backup destination (backup.OpenDestination) to fetch
	// the snapshot from.
	Source blob.Adapter

	// SnapshotKey names which snapshot to restore. Empty resolves to the
	// newest object in Source (backup.LatestSnapshotKey).
	SnapshotKey string

	// Identity is the bundle's age backup key: it decrypts the fetched
	// snapshot (BKUP-003, BKUP-004).
	Identity *age.X25519Identity

	// Keystore is the target keystore driver key material is installed
	// into. It must implement keystore.Writer — the same requirement
	// initialize.Run places on the keystore target it's given.
	Keystore keystore.Driver

	// Blobs is the target blob.Adapter blobs are restored into (STATE-003).
	Blobs blob.Adapter

	// Host is the already-connected session to the fresh target host.
	Host Host

	// CertIssuer is passed through to deploy.Up unchanged; nil uses the
	// real ACME-backed issuer.
	CertIssuer deploy.CertIssuer
}

func (o Options) validate() error {
	if strings.TrimSpace(o.WorkDir) == "" {
		return errors.New("restore: work directory is required")
	}
	if strings.TrimSpace(o.RemoteDir) == "" {
		return errors.New("restore: remote directory is required")
	}
	if o.Bundle == nil {
		return errors.New("restore: bundle is required")
	}
	if o.Source == nil {
		return errors.New("restore: snapshot source is required")
	}
	if o.Identity == nil {
		return errors.New("restore: age identity is required")
	}
	if o.Keystore == nil {
		return errors.New("restore: keystore driver is required")
	}
	if o.Blobs == nil {
		return errors.New("restore: blob adapter is required")
	}
	if o.Host == nil {
		return errors.New("restore: host is required")
	}
	return nil
}

// keyNames returns the fixed set of key material names every bundle
// carries (state.KeyExporter.Names()) — Names never actually reads its
// receiver's Driver field, so a zero-value KeystoreKeyExporter is enough
// to ask for it without a real target keystore driver in hand yet.
func keyNames() []string {
	return (&state.KeystoreKeyExporter{}).Names()
}

// Restore runs RSTR-001 end to end as one CORE-002 job: fetch, decrypt, and
// verify the named (or newest) snapshot in opts.Source; install its key
// material into opts.Keystore; place its git data and database onto
// opts.Host under opts.RemoteDir; restore its blobs into opts.Blobs; and
// converge opts.Host to opts.Bundle's definition against that state
// (deploy.Up).
//
// Restore owns job's terminal event, calling job.Succeeded or job.Failed
// exactly once, after every step below has run or the first one fails.
func Restore(ctx context.Context, job *events.Job, opts Options) error {
	manifest, err := restore(ctx, job, opts)
	if err != nil {
		job.Failed(err.Error())
		return err
	}
	job.Succeeded(fmt.Sprintf("instance restored from a snapshot captured by forgejo %s", manifest.ForgejoVersion))
	return nil
}

func restore(ctx context.Context, job *events.Job, opts Options) (*backup.Manifest, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.WorkDir, 0o700); err != nil {
		return nil, fmt.Errorf("restore: create work directory: %w", err)
	}
	defer os.RemoveAll(opts.WorkDir)

	archivePath := filepath.Join(opts.WorkDir, "snapshot.age")
	if err := fetchSnapshot(ctx, job, opts, archivePath); err != nil {
		return nil, err
	}

	plainDir := filepath.Join(opts.WorkDir, "snapshot")
	manifest, err := decryptAndVerify(ctx, job, archivePath, plainDir, opts.Identity)
	if err != nil {
		return nil, err
	}

	if err := installKeys(ctx, job, plainDir, manifest, opts.Keystore); err != nil {
		return nil, err
	}

	if err := placeState(ctx, job, plainDir, manifest, opts); err != nil {
		return nil, err
	}

	if err := restoreBlobs(ctx, job, plainDir, manifest, opts.Blobs); err != nil {
		return nil, err
	}

	if err := runDeploy(ctx, job, manifest, opts); err != nil {
		return nil, err
	}

	return manifest, nil
}

// fetchSnapshot resolves opts.SnapshotKey (or the newest object in
// opts.Source) and downloads it to archivePath.
func fetchSnapshot(ctx context.Context, job *events.Job, opts Options, archivePath string) error {
	job.Started(StepFetch, "fetching snapshot")

	key := opts.SnapshotKey
	if key == "" {
		var err error
		key, err = backup.LatestSnapshotKey(ctx, opts.Source)
		if err != nil {
			err = fmt.Errorf("restore: %w", err)
			job.Emit(StepFetch, events.StateFailed, err.Error())
			return err
		}
	}
	if err := backup.Fetch(ctx, opts.Source, key, archivePath); err != nil {
		err = fmt.Errorf("restore: fetch snapshot: %w", err)
		job.Emit(StepFetch, events.StateFailed, err.Error())
		return err
	}
	job.Emit(StepFetch, events.StateSucceeded, fmt.Sprintf("fetched snapshot %s", key))
	return nil
}

// decryptAndVerify decrypts archivePath into plainDir and runs backup.Verify
// against it, refusing loudly — naming every defect found — rather than
// restoring a torn or corrupt snapshot (RSTR-003; CLAUDE.md "verification
// is load-bearing").
func decryptAndVerify(ctx context.Context, job *events.Job, archivePath, plainDir string, identity age.Identity) (*backup.Manifest, error) {
	job.Started(StepDecrypt, "decrypting snapshot")
	if err := backup.DecryptArchive(ctx, archivePath, plainDir, identity); err != nil {
		err = fmt.Errorf("restore: decrypt snapshot: %w", err)
		job.Emit(StepDecrypt, events.StateFailed, err.Error())
		return nil, err
	}
	job.Emit(StepDecrypt, events.StateSucceeded, "snapshot decrypted")

	job.Started(StepVerify, "verifying snapshot")
	manifest, err := backup.ReadManifest(plainDir)
	if err != nil {
		err = fmt.Errorf("restore: %w", err)
		job.Emit(StepVerify, events.StateFailed, err.Error())
		return nil, err
	}
	if err := backup.Verify(ctx, plainDir, manifest, keyNames()); err != nil {
		err = fmt.Errorf("restore: snapshot failed verification, refusing to restore: %w", err)
		job.Emit(StepVerify, events.StateFailed, err.Error())
		return nil, err
	}
	job.Emit(StepVerify, events.StateSucceeded, "snapshot verified")
	return manifest, nil
}

// runDeploy runs deploy.Up against a private job, relaying every step event
// it emits onto job as it happens — the same relay backup.Backup's
// runCapture uses for backup.Run, needed for the same reason: deploy.Up
// ends whatever job it's given (its own documented, tested contract), so a
// shared job here would close job's stream the moment Up finishes and
// panic on every Emit call afterward — there are none after Up in Restore,
// but the relay also keeps job's own terminal event Restore's alone to
// decide, matching every other multi-step job in this codebase.
//
// It deploys pinnedBundle(opts.Bundle, manifest.ForgejoVersion) rather than
// opts.Bundle itself (RSTR-002): the target bundle's own farrier.yaml may
// pin a different Forgejo image than the one the snapshot was captured
// from — e.g. an upgrade run against the bundle since the snapshot was
// taken — and restore must boot the exact version recorded in the
// snapshot every time (spec.md "Version pinning").
func runDeploy(ctx context.Context, job *events.Job, manifest *backup.Manifest, opts Options) error {
	deployBundle, err := pinnedBundle(opts.Bundle, manifest.ForgejoVersion)
	if err != nil {
		return err
	}

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

	err = deploy.Up(ctx, deployJob, opts.Host, deployBundle, deploy.Options{
		RemoteDir:  opts.RemoteDir,
		CertIssuer: opts.CertIssuer,
	})
	<-relayed
	return err
}

// pinnedBundle returns a copy of b whose forge image is overridden to
// forgejoVersion — the image ref the snapshot manifest recorded (BKUP-001,
// captured from the source bundle's own Manifest.Images at backup time,
// backup.BuildOptions) — with Compose re-rendered from that override the
// same way orchestrate.Render already builds b's Compose at init time.
// Compose is re-rendered, not just the manifest, because deploy.Up ships
// b.Compose to the host as-is (deploy.configureForge); overriding only the
// manifest would leave the stale image in the Compose file deploy.Up
// actually converges the host to.
//
// b itself is left untouched: Images is copied before the override so the
// caller's own bundle (and the map backing its Manifest.Images) is never
// mutated out from under it.
func pinnedBundle(b *bundle.Bundle, forgejoVersion string) (*bundle.Bundle, error) {
	manifest := b.Manifest
	images := make(map[string]string, len(manifest.Images))
	for component, image := range manifest.Images {
		images[component] = image
	}
	images[forge.Service] = forgejoVersion
	manifest.Images = images

	compose, err := orchestrate.Render(&manifest)
	if err != nil {
		return nil, fmt.Errorf("restore: render compose pinned to forgejo %s: %w", forgejoVersion, err)
	}
	return &bundle.Bundle{Manifest: manifest, Compose: compose}, nil
}
