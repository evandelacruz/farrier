// Package backup implements BKUP-001 and BKUP-002: capturing all four kinds
// of state — git data (state.GitExporter), the database
// (state.DatabaseExporter), blobs (state.BlobExporter), and key material
// (state.KeyExporter) — into a snapshot directory, with a manifest
// (snapshot-manifest.json) recording the Forgejo version, the capture
// timestamp, and a checksum for every captured file (tech-spec.md "Snapshot
// format"). It is the core logic behind the `backup` CLI command.
//
// Run holds git pushes (PushHold) only across the database backup and
// recording every repository's ref state — the one part of git data a push
// actually changes — and releases before tarring the (immutable,
// append-only) object store, so the hold stays database-only regardless of
// how much git data the instance holds (BKUP-002, docs/spec.md "Backups").
//
// Run produces a plain, unencrypted snapshot directory. Encryption
// (BKUP-003), verification at creation (BKUP-004), and writing the result
// to an S3-compatible URI or filesystem path (BKUP-005) are separate
// requirements layered on top of what Run produces here.
package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// Step names emitted through the job's event stream (CORE-002).
const (
	StepValidate      = "validate"
	StepPushHold      = "push-hold"
	StepDatabase      = "capture-database"
	StepRecordRefs    = "record-refs"
	StepGit           = "capture-git"
	StepBlobs         = "capture-blobs"
	StepKeys          = "capture-keys"
	StepWriteManifest = "write-manifest"
)

// defaultPushHoldCeiling bounds the push hold when Params.PushHoldCeiling
// isn't set: a bug backstop, not a growth alarm — with the object tar
// happening outside the hold, the hold itself no longer grows with the
// instance, so a low default is always appropriate (BKUP-002).
const defaultPushHoldCeiling = 30 * time.Second

// Params are backup's inputs: where to write the snapshot, the Forgejo
// version that produced it, and the four state exporters Run captures from.
type Params struct {
	// Dir is the directory Run writes the plain (not yet encrypted)
	// snapshot to. Run creates it if it doesn't already exist.
	Dir string

	// ForgejoVersion is recorded in the manifest as the exact version this
	// snapshot was captured from (spec.md "Version pinning").
	ForgejoVersion string

	Git         state.GitExporter
	GitCapturer GitCapturer
	Database    state.DatabaseExporter
	Blobs       state.BlobExporter
	Keys        state.KeyExporter

	// PushHold rejects git pushes for the database-only window Run holds
	// them across: SQLite's online backup plus recording every
	// repository's ref state (BKUP-002).
	PushHold PushHold

	// PushHoldCeiling bounds how long that window may run before Run gives
	// up and releases anyway. Zero uses defaultPushHoldCeiling.
	PushHoldCeiling time.Duration
}

func (p Params) pushHoldCeiling() time.Duration {
	if p.PushHoldCeiling > 0 {
		return p.PushHoldCeiling
	}
	return defaultPushHoldCeiling
}

// Run captures all four kinds of state into params.Dir and writes
// snapshot-manifest.json there, emitting CORE-002 progress events on job as
// it goes. It returns the manifest it wrote, or an error — with job
// carrying a StateFailed event either way, so a caller only needs to check
// the returned error, not separately inspect the event stream, to know
// whether backup succeeded.
func Run(ctx context.Context, job *events.Job, params Params) (*Manifest, error) {
	job.Started(StepValidate, "checking snapshot parameters")
	if err := params.validate(); err != nil {
		return fail(job, StepValidate, err)
	}
	if err := os.MkdirAll(params.Dir, 0o700); err != nil {
		return fail(job, StepValidate, fmt.Errorf("backup: create snapshot directory: %w", err))
	}
	job.Emit(StepValidate, events.StateSucceeded, "snapshot parameters are valid")

	var components []Component

	dbComponent, refComponents, remotes, err := captureUnderHold(ctx, job, params)
	if err != nil {
		// captureUnderHold has already emitted the one step event that
		// attributes this failure (StepPushHold for an Engage/Release
		// error, StepDatabase or StepRecordRefs for a capture error under
		// the hold) — failJob only marks the job terminal, so a failure
		// here still produces exactly one failed step in the CORE-002
		// stream.
		return failJob(job, err)
	}
	job.Emit(StepPushHold, events.StateSucceeded, "pushes released")
	components = append(components, dbComponent)
	components = append(components, refComponents...)

	job.Started(StepGit, "capturing git repositories")
	gitComponents, err := captureGitObjects(ctx, params.Dir, params.GitCapturer, remotes)
	if err != nil {
		return fail(job, StepGit, err)
	}
	components = append(components, gitComponents...)
	job.Emit(StepGit, events.StateSucceeded, fmt.Sprintf("captured %d repository(ies)", len(gitComponents)))

	job.Started(StepBlobs, "capturing blobs")
	blobComponents, err := captureBlobs(ctx, params.Dir, params.Blobs)
	if err != nil {
		return fail(job, StepBlobs, err)
	}
	components = append(components, blobComponents...)
	job.Emit(StepBlobs, events.StateSucceeded, fmt.Sprintf("captured %d blob(s)", len(blobComponents)))

	job.Started(StepKeys, "capturing key material")
	keyComponents, err := captureKeys(ctx, params.Dir, params.Keys)
	if err != nil {
		return fail(job, StepKeys, err)
	}
	components = append(components, keyComponents...)
	job.Emit(StepKeys, events.StateSucceeded, fmt.Sprintf("captured %d key(s)", len(keyComponents)))

	job.Started(StepWriteManifest, "writing snapshot manifest")
	manifest := &Manifest{
		ForgejoVersion:    params.ForgejoVersion,
		Timestamp:         time.Now().UTC(),
		ChecksumAlgorithm: bundle.DefaultChecksumAlgorithm,
		Components:        components,
	}
	if err := writeManifest(params.Dir, manifest); err != nil {
		return fail(job, StepWriteManifest, err)
	}
	job.Emit(StepWriteManifest, events.StateSucceeded, fmt.Sprintf("manifest written with %d component(s)", len(components)))

	job.Succeeded(fmt.Sprintf("snapshot written to %s", params.Dir))
	return manifest, nil
}

func (p Params) validate() error {
	if strings.TrimSpace(p.Dir) == "" {
		return errors.New("backup: snapshot directory is required")
	}
	if strings.TrimSpace(p.ForgejoVersion) == "" {
		return errors.New("backup: forgejo version is required")
	}
	if p.Git == nil {
		return errors.New("backup: git exporter is required")
	}
	if p.GitCapturer == nil {
		return errors.New("backup: git capturer is required")
	}
	if p.Database == nil {
		return errors.New("backup: database exporter is required")
	}
	if p.Blobs == nil {
		return errors.New("backup: blob exporter is required")
	}
	if p.Keys == nil {
		return errors.New("backup: key exporter is required")
	}
	if p.PushHold == nil {
		return errors.New("backup: push hold is required")
	}
	return nil
}

// captureUnderHold engages params.PushHold, captures the database and
// records every repository's ref state while it's engaged, and releases the
// hold before returning — on success, on error, or if this panics — so a
// capture that dies mid-hold never leaves pushes rejected (BKUP-002,
// docs/spec.md "Backups"). The object tar is deliberately not captured
// here: Run only starts it after this returns, once pushes are flowing
// again.
func captureUnderHold(ctx context.Context, job *events.Job, params Params) (dbComponent Component, refComponents []Component, remotes []state.Remote, err error) {
	job.Started(StepPushHold, "holding git pushes")
	if err = params.PushHold.Engage(ctx); err != nil {
		err = fmt.Errorf("backup: engage push hold: %w", err)
		job.Emit(StepPushHold, events.StateFailed, err.Error())
		return
	}

	holdCtx, cancel := context.WithTimeout(ctx, params.pushHoldCeiling())
	defer cancel()

	defer func() {
		// Release unconditionally, on a context detached from ctx's own
		// cancellation (and from the ceiling above) — a caller that
		// cancels ctx, or a ceiling that expires, must not also be able
		// to block the release that has to follow it.
		releaseErr := params.PushHold.Release(context.WithoutCancel(ctx))
		if releaseErr == nil {
			return
		}
		releaseErr = fmt.Errorf("backup: release push hold: %w", releaseErr)
		if err != nil {
			// The capture underneath already emitted its own failed step
			// (StepDatabase or StepRecordRefs) — the release failure is
			// folded into that same error, not attributed to a step of
			// its own, so this still produces exactly one failed step.
			err = fmt.Errorf("%w (also failed to release push hold: %v)", err, releaseErr)
			return
		}
		err = releaseErr
		job.Emit(StepPushHold, events.StateFailed, err.Error())
	}()

	job.Started(StepDatabase, "capturing database")
	dbComponent, err = captureDatabase(holdCtx, params.Dir, params.Database)
	if err != nil {
		job.Emit(StepDatabase, events.StateFailed, err.Error())
		return
	}
	job.Emit(StepDatabase, events.StateSucceeded, "database captured")

	job.Started(StepRecordRefs, "recording repository refs")
	remotes, err = params.Git.Remotes(holdCtx)
	if err != nil {
		err = fmt.Errorf("backup: capture git: list repositories: %w", err)
		job.Emit(StepRecordRefs, events.StateFailed, err.Error())
		return
	}
	refComponents, err = captureGitRefs(holdCtx, params.Dir, params.GitCapturer, remotes)
	if err != nil {
		job.Emit(StepRecordRefs, events.StateFailed, err.Error())
		return
	}
	job.Emit(StepRecordRefs, events.StateSucceeded, fmt.Sprintf("recorded refs for %d repository(ies)", len(remotes)))
	return
}

func fail(job *events.Job, step string, err error) (*Manifest, error) {
	job.Emit(step, events.StateFailed, err.Error())
	job.Failed(err.Error())
	return nil, err
}

// failJob marks job terminally failed without emitting an additional step
// event — for a caller whose failure path has already attributed exactly
// one step itself (captureUnderHold).
func failJob(job *events.Job, err error) (*Manifest, error) {
	job.Failed(err.Error())
	return nil, err
}
