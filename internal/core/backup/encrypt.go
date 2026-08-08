package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"github.com/evandelacruz/farrier/internal/core/events"
)

// StepEncrypt identifies the encryption step in a backup job's event
// stream.
const StepEncrypt = "encrypt"

// Encrypt archives the plain snapshot directory Run wrote at dir — the
// manifest plus every captured component (tech-spec.md "Snapshot format")
// — as a single tar stream, age-encrypts it to recipient, and writes the
// result to destPath: the one age-encrypted archive per backup BKUP-003
// requires, so nothing captured ever leaves the host unencrypted. The
// operator's age identity holds the only key that can open it back up
// (spec.md "Key custody") — Encrypt itself never sees or needs the
// identity, only its public recipient half.
//
// Encrypt emits its own StepEncrypt event on job but, like forge.Bootstrap
// and forge.ReconcileCI, does not end job: a backup job composes Run,
// Encrypt, and Write (and, once it lands, BKUP-004's verification) under
// one job whose terminal event the caller owns.
//
// On failure Encrypt removes any partial file it left at destPath — a
// truncated archive there could otherwise be mistaken for a real backup.
func Encrypt(ctx context.Context, job *events.Job, dir, destPath string, recipient age.Recipient) error {
	job.Started(StepEncrypt, "encrypting snapshot")

	if strings.TrimSpace(dir) == "" {
		return failEncrypt(job, errors.New("backup: encrypt: snapshot directory is required"))
	}
	if strings.TrimSpace(destPath) == "" {
		return failEncrypt(job, errors.New("backup: encrypt: destination path is required"))
	}
	if recipient == nil {
		return failEncrypt(job, errors.New("backup: encrypt: recipient is required"))
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return failEncrypt(job, fmt.Errorf("backup: encrypt: create destination directory: %w", err))
	}
	f, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return failEncrypt(job, fmt.Errorf("backup: encrypt: create %s: %w", destPath, err))
	}

	if err := encryptTo(ctx, f, dir, recipient); err != nil {
		f.Close()
		os.Remove(destPath)
		return failEncrypt(job, fmt.Errorf("backup: encrypt: %w", err))
	}
	if err := f.Close(); err != nil {
		os.Remove(destPath)
		return failEncrypt(job, fmt.Errorf("backup: encrypt: %s: %w", destPath, err))
	}

	job.Emit(StepEncrypt, events.StateSucceeded, fmt.Sprintf("snapshot encrypted to %s", destPath))
	return nil
}

// encryptTo tars every file under dir and age-encrypts the result to f, one
// recipient. The tar stream is written straight into age's encrypting
// writer — dir's plaintext bytes are never staged as a second copy on disk.
func encryptTo(ctx context.Context, f *os.File, dir string, recipient age.Recipient) error {
	w, err := age.Encrypt(f, recipient)
	if err != nil {
		return err
	}
	if err := tarDir(ctx, w, dir); err != nil {
		w.Close()
		return fmt.Errorf("archive snapshot: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finalize encryption: %w", err)
	}
	return nil
}

// failEncrypt emits a StateFailed StepEncrypt event and returns err
// unchanged. It does not end job (see Encrypt's doc comment) — the caller's
// backup job owns that.
func failEncrypt(job *events.Job, err error) error {
	job.Emit(StepEncrypt, events.StateFailed, err.Error())
	return err
}
