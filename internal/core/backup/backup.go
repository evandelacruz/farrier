// Package backup implements BKUP-001: capturing all four kinds of state —
// git data (state.GitExporter), the database (state.DatabaseExporter),
// blobs (state.BlobExporter), and key material (state.KeyExporter) — into a
// snapshot directory, with a manifest (snapshot-manifest.json) recording the
// Forgejo version, the capture timestamp, and a checksum for every captured
// file (tech-spec.md "Snapshot format"). It is the core logic behind the
// `backup` CLI command.
//
// Run produces a plain, unencrypted snapshot directory; Encrypt (BKUP-003)
// turns it into the single age-encrypted archive that actually leaves the
// host. The push-hold window around git capture (BKUP-002), verification at
// creation (BKUP-004), and writing the result to an S3-compatible URI or
// filesystem path (BKUP-005) are separate requirements layered on top of
// what Run and Encrypt produce here.
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
	StepDatabase      = "capture-database"
	StepGit           = "capture-git"
	StepBlobs         = "capture-blobs"
	StepKeys          = "capture-keys"
	StepWriteManifest = "write-manifest"
)

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

	job.Started(StepDatabase, "capturing database")
	dbComponent, err := captureDatabase(ctx, params.Dir, params.Database)
	if err != nil {
		return fail(job, StepDatabase, err)
	}
	components = append(components, dbComponent)
	job.Emit(StepDatabase, events.StateSucceeded, "database captured")

	job.Started(StepGit, "capturing git repositories")
	gitComponents, err := captureGit(ctx, params.Dir, params.Git, params.GitCapturer)
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
	return nil
}

func fail(job *events.Job, step string, err error) (*Manifest, error) {
	job.Emit(step, events.StateFailed, err.Error())
	job.Failed(err.Error())
	return nil, err
}
