// orchestrate.go implements BKUP-006: composing OpenDestination, Run,
// Encrypt, Verify, and Write — each already a complete, independently
// tested step — into the single operator-invokable `backup` command
// (tech-spec.md "Snapshot orchestration"). It is the first real caller of
// any of BKUP-001 through BKUP-005.
package backup

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/initialize"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// Step identifiers Backup itself emits through the job's event stream,
// beyond the ones Run, Encrypt, and Write already own. StepVerifyEncrypted
// is deliberately distinct from Run's own StepVerify: Run's verify runs
// against the plain, pre-encryption directory it just captured; this one
// runs again after Encrypt, against the decrypted form of the exact bytes
// Write is about to send off the host (tech-spec.md "Snapshot
// verification": "verify" ultimately runs after "encrypt").
const (
	StepResolveDestination = "resolve-destination"
	StepVerifyEncrypted    = "verify-encrypted"
)

// Options configures Backup: everything OpenDestination, Run, Encrypt, and
// Verify need beyond what each already resolves on its own.
type Options struct {
	// WorkDir is the local scratch directory Backup captures the plain
	// snapshot into, stages the encrypted archive in, and decrypts it back
	// into for the post-encrypt verify pass. Backup creates it if it
	// doesn't exist and removes it — and everything under it — before
	// returning, on both the success and failure paths: a plaintext
	// snapshot is never left on disk once Backup returns.
	WorkDir string

	// ForgejoVersion is recorded in the snapshot manifest (BKUP-001).
	ForgejoVersion string

	// Destination is the operator-named backup target — an S3-compatible
	// URI or a filesystem path — OpenDestination resolves (BKUP-005).
	Destination string

	// Identity is the bundle's age backup key (initialize.KeyAgeBackupKey):
	// its Recipient half encrypts the snapshot (BKUP-003), and Identity
	// itself decrypts it again for the post-encrypt verify pass (BKUP-004).
	Identity *age.X25519Identity

	Git             state.GitExporter
	GitCapturer     GitCapturer
	Database        state.DatabaseExporter
	Blobs           state.BlobExporter
	Keys            state.KeyExporter
	PushHold        PushHold
	PushHoldCeiling time.Duration
}

func (o Options) validate() error {
	if strings.TrimSpace(o.WorkDir) == "" {
		return errors.New("backup: work directory is required")
	}
	if o.Identity == nil {
		return errors.New("backup: age identity is required")
	}
	return nil
}

// Backup runs BKUP-001 through BKUP-005 end to end as one CORE-002 job
// (BKUP-006): it resolves opts.Destination into a blob.Adapter, captures
// every kind of state (Run), age-encrypts the capture into a single
// archive (Encrypt), verifies that archive's decrypted form before
// anything already written survives as the record of a successful backup
// (Verify, run again here against Encrypt's output — Run already verified
// the plain, pre-encryption capture on its own), and writes it to the
// resolved destination (Write).
//
// Every step reports through job's event stream; Backup owns job's
// terminal event, calling job.Succeeded or job.Failed exactly once, after
// every step below has run or the first one fails — the same shape
// deploy.Up already uses for `up`. Run, Encrypt, and Write each already
// emit their own step events on the job they're given without ending it,
// so Encrypt, Verify, and Write receive job directly; Run does not share
// that contract yet (it still ends whatever job it's given, matching its
// own existing tests), so Backup gives it a private job of its own and
// relays its step events onto job as they happen, leaving job's own
// terminal event for Backup alone to decide.
func Backup(ctx context.Context, job *events.Job, opts Options) error {
	key, err := backup(ctx, job, opts)
	if err != nil {
		job.Failed(err.Error())
		return err
	}
	job.Succeeded(fmt.Sprintf("backup written to %s", key))
	return nil
}

func backup(ctx context.Context, job *events.Job, opts Options) (string, error) {
	if err := opts.validate(); err != nil {
		return "", err
	}
	defer os.RemoveAll(opts.WorkDir)

	job.Started(StepResolveDestination, "resolving backup destination")
	dest, err := OpenDestination(opts.Destination)
	if err != nil {
		job.Emit(StepResolveDestination, events.StateFailed, err.Error())
		return "", err
	}
	job.Emit(StepResolveDestination, events.StateSucceeded, "destination resolved")

	plainDir := filepath.Join(opts.WorkDir, "snapshot")
	manifest, err := runCapture(ctx, job, plainDir, opts)
	if err != nil {
		return "", err
	}

	archivePath := filepath.Join(opts.WorkDir, "snapshot.age")
	if err := Encrypt(ctx, job, plainDir, archivePath, opts.Identity.Recipient()); err != nil {
		return "", err
	}

	decryptedDir := filepath.Join(opts.WorkDir, "verify")
	if err := verifyEncrypted(ctx, job, archivePath, decryptedDir, opts.Identity, manifest, opts.Keys.Names()); err != nil {
		return "", err
	}

	key, err := Write(ctx, job, dest, archivePath, manifest.Timestamp)
	if err != nil {
		return "", err
	}
	return key, nil
}

// ResolveIdentity reads the bundle's age backup key
// (initialize.KeyAgeBackupKey, BKUP-003) through driver and parses it into
// the identity Options.Identity needs — the same identity init generated
// and persisted, never regenerated or derived here. Both the `backup` CLI
// command and POST /backup call this rather than each parsing the secret
// themselves, so the two frontends share the one piece of real logic
// resolving Options.Identity takes (CLAUDE.md "one core, thin skins").
func ResolveIdentity(ctx context.Context, driver keystore.Driver) (*age.X25519Identity, error) {
	secret, err := driver.Resolve(ctx, initialize.KeyAgeBackupKey)
	if err != nil {
		return nil, fmt.Errorf("backup: resolve age backup key: %w", err)
	}
	identity, err := age.ParseX25519Identity(secret.Reveal())
	if err != nil {
		return nil, fmt.Errorf("backup: parse age backup key: %w", err)
	}
	return identity, nil
}

// runCapture runs Run against a private job, relaying every step event it
// emits onto job as it happens — live, not buffered until Run returns —
// while leaving job's own terminal event untouched: Run ends whatever job
// it's given (its own documented, tested contract), so a shared job here
// would close job's stream the moment Run finishes and panic on every
// Emit call Encrypt, Verify, or Write made afterward.
func runCapture(ctx context.Context, job *events.Job, dir string, opts Options) (*Manifest, error) {
	runJob := events.NewJob()
	stream, cancel := runJob.Subscribe()
	defer cancel()

	relayed := make(chan struct{})
	go func() {
		defer close(relayed)
		for ev := range stream {
			if ev.Step == "" {
				// runJob's own terminal event: it reports Run's outcome on
				// runJob, not job — job's terminal event is Backup's alone
				// to decide, once every later step has also run.
				continue
			}
			job.Emit(ev.Step, ev.State, ev.Detail)
		}
	}()

	manifest, err := Run(ctx, runJob, Params{
		Dir:             dir,
		ForgejoVersion:  opts.ForgejoVersion,
		Git:             opts.Git,
		GitCapturer:     opts.GitCapturer,
		Database:        opts.Database,
		Blobs:           opts.Blobs,
		Keys:            opts.Keys,
		PushHold:        opts.PushHold,
		PushHoldCeiling: opts.PushHoldCeiling,
	})
	<-relayed
	if err != nil {
		return nil, err
	}
	return manifest, nil
}

// verifyEncrypted decrypts archivePath — the exact bytes Write is about to
// send off the host — into destDir and runs Verify against that decrypted
// form, so a backup that fails verification fails loudly before anything
// leaves the host, checked against what actually leaves rather than only
// the pre-encryption capture Run already verified on its own.
func verifyEncrypted(ctx context.Context, job *events.Job, archivePath, destDir string, identity age.Identity, manifest *Manifest, keyNames []string) error {
	job.Started(StepVerifyEncrypted, "verifying encrypted snapshot")

	if err := DecryptArchive(ctx, archivePath, destDir, identity); err != nil {
		err = fmt.Errorf("backup: decrypt snapshot for verification: %w", err)
		job.Emit(StepVerifyEncrypted, events.StateFailed, err.Error())
		return err
	}
	if err := Verify(ctx, destDir, manifest, keyNames); err != nil {
		err = fmt.Errorf("backup: verify encrypted snapshot: %w", err)
		job.Emit(StepVerifyEncrypted, events.StateFailed, err.Error())
		return err
	}
	job.Emit(StepVerifyEncrypted, events.StateSucceeded, "encrypted snapshot verified")
	return nil
}

// DecryptArchive decrypts the age archive at archivePath with identity and
// extracts its tar contents into destDir, undoing exactly what encryptTo
// did — the same tar format tarDir's writer produces. verifyEncrypted uses
// it to check the exact bytes Write is about to send off the host;
// restore.Restore uses the same function to turn a fetched snapshot back
// into a plain directory Verify can check before anything from it is
// installed anywhere (tech-spec.md "Snapshot format": "Verification at
// creation and at restore runs the same code path").
func DecryptArchive(ctx context.Context, archivePath, destDir string, identity age.Identity) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	r, err := age.Decrypt(f, identity)
	if err != nil {
		return fmt.Errorf("age decrypt: %w", err)
	}
	return untar(ctx, r, destDir)
}

// untar extracts r's tar entries into destDir, recreating directories and
// files with names relative to destDir. It refuses any entry whose name
// would resolve outside destDir, defense in depth against a corrupted or
// tampered archive even though every archive Backup decrypts here is one
// it encrypted moments earlier from its own tarDir output.
func untar(ctx context.Context, r io.Reader, destDir string) error {
	root := filepath.Clean(destDir)
	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		target := filepath.Join(root, filepath.FromSlash(hdr.Name))
		if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return fmt.Errorf("tar entry %q escapes destination", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
}
