package state

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// databaseWaitDelay bounds how long Snapshot waits for the backup command's
// stdout/stderr to close after the process exits, the same protection
// dns.RFC2136Driver and keystore.CommandDriver apply to their own external
// commands.
const databaseWaitDelay = 2 * time.Second

// defaultSQLiteCommand is the sqlite3 executable resolved via PATH when no
// override is configured.
const defaultSQLiteCommand = "sqlite3"

// DatabaseExporter exposes Forgejo's SQLite database as a consistent,
// point-in-time snapshot (STATE-002, spec.md "Database"): a copy taken
// through SQLite's own Online Backup API — invoked here via the sqlite3
// CLI's ".backup" meta-command, the shell-facing name for that same API —
// so the live database is never paused and readers and writers are never
// blocked while the copy is made.
type DatabaseExporter interface {
	// Snapshot returns a stream over a consistent copy of the database.
	// The caller must Close the returned reader; Close also removes any
	// temporary file the exporter created to hold the copy.
	Snapshot(ctx context.Context) (io.ReadCloser, error)
}

// LocalDatabaseExporter backs up a SQLite database file reachable directly
// on disk — the drill path (DRIL-001), and anywhere else the database file
// is reachable without SSH — by shelling out to the sqlite3 CLI, the same
// external-command approach dns.RFC2136Driver takes with nsupdate and
// keystore.CommandDriver takes with an operator-supplied command.
type LocalDatabaseExporter struct {
	// Path is the database file's location on disk.
	Path string

	// Command overrides the sqlite3 executable; empty resolves "sqlite3"
	// via PATH.
	Command string
}

// Snapshot runs `sqlite3 Path ".backup <tmp>"` and returns the resulting
// file as a stream.
func (e *LocalDatabaseExporter) Snapshot(ctx context.Context) (io.ReadCloser, error) {
	if strings.TrimSpace(e.Path) == "" {
		return nil, errors.New("state: local database exporter: path is required")
	}

	destPath, err := reserveTempSnapshot()
	if err != nil {
		return nil, fmt.Errorf("state: local database exporter: %w", err)
	}

	command := e.Command
	if command == "" {
		command = defaultSQLiteCommand
	}

	cmd := exec.CommandContext(ctx, command, e.Path, backupDotCommand(destPath))
	cmd.WaitDelay = databaseWaitDelay
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		os.Remove(destPath)
		if ctx.Err() != nil {
			return nil, fmt.Errorf("state: local database exporter: %w", ctx.Err())
		}
		return nil, fmt.Errorf("state: local database exporter: %s: %w%s", command, err, commandStderrSuffix(stderr.String()))
	}

	return openTempSnapshot(destPath)
}

// SSHDatabaseExporter backs up a SQLite database running inside a Docker
// container on a remote forge host, over an already-connected Runner (an
// *orchestrate.Client in production). It reaches the database with `docker
// exec` rather than assuming a bare sqlite3 install on the host itself,
// since Docker and SSH are the only things ORCH-001 guarantees a host
// provides — the sqlite3 CLI only needs to exist inside the pinned Forgejo
// image, which Farrier already controls.
type SSHDatabaseExporter struct {
	Runner Runner

	// Container is the name of the running Docker container hosting
	// Forgejo (e.g. "farrier-forge").
	Container string

	// Path is the database file's location inside that container.
	Path string
}

// Snapshot runs `docker exec <container> sqlite3 Path ".backup <tmp>"`
// inside the container, streams the resulting file back over the same SSH
// session, and removes the container-side temporary file before returning
// — success or failure.
func (e *SSHDatabaseExporter) Snapshot(ctx context.Context) (io.ReadCloser, error) {
	if e.Runner == nil {
		return nil, errors.New("state: ssh database exporter: runner is required")
	}
	if strings.TrimSpace(e.Container) == "" {
		return nil, errors.New("state: ssh database exporter: container is required")
	}
	if strings.TrimSpace(e.Path) == "" {
		return nil, errors.New("state: ssh database exporter: path is required")
	}

	remoteTmp, err := randomHex(8)
	if err != nil {
		return nil, fmt.Errorf("state: ssh database exporter: %w", err)
	}
	remotePath := fmt.Sprintf("/tmp/farrier-db-snapshot-%s.db", remoteTmp)

	destPath, err := reserveTempSnapshot()
	if err != nil {
		return nil, fmt.Errorf("state: ssh database exporter: %w", err)
	}
	dest, err := os.OpenFile(destPath, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		os.Remove(destPath)
		return nil, fmt.Errorf("state: ssh database exporter: open temp file: %w", err)
	}

	// Backup, then stream the result out, then remove the container-side
	// copy regardless of whether the cat succeeded — a single command so
	// the cleanup can't be skipped by a caller that stops after Snapshot
	// returns an error.
	command := fmt.Sprintf(
		"docker exec %s sqlite3 %s %s >/dev/null && docker exec %s cat %s; rc=$?; docker exec %s rm -f %s >/dev/null 2>&1; exit $rc",
		shellQuote(e.Container), shellQuote(e.Path), shellQuote(backupDotCommand(remotePath)),
		shellQuote(e.Container), shellQuote(remotePath),
		shellQuote(e.Container), shellQuote(remotePath),
	)

	var stderr strings.Builder
	runErr := e.Runner.Run(ctx, command, dest, &stderr)
	closeErr := dest.Close()
	if runErr != nil {
		os.Remove(destPath)
		return nil, fmt.Errorf("state: ssh database exporter: %s: %w%s", e.Container, runErr, commandStderrSuffix(stderr.String()))
	}
	if closeErr != nil {
		os.Remove(destPath)
		return nil, fmt.Errorf("state: ssh database exporter: %w", closeErr)
	}

	return openTempSnapshot(destPath)
}

// backupDotCommand builds the sqlite3 ".backup" meta-command that copies
// the currently open database to dest using the Online Backup API.
func backupDotCommand(dest string) string {
	return fmt.Sprintf(".backup '%s'", strings.ReplaceAll(dest, "'", `'\''`))
}

func commandStderrSuffix(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return fmt.Sprintf(" (stderr: %s)", stderr)
}

// randomHex returns n random bytes hex-encoded, used to keep concurrent
// Snapshot calls against the same container from colliding on the
// container-side temporary path.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random suffix: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// reserveTempSnapshot creates an empty, uniquely named file to hold one
// Snapshot call's output and returns its path. os.CreateTemp mode (0600)
// keeps database contents unreadable to other local users while the
// snapshot exists.
func reserveTempSnapshot() (string, error) {
	f, err := os.CreateTemp("", "farrier-db-snapshot-*.db")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("create temp file: %w", err)
	}
	return path, nil
}

// tempFileReadCloser wraps a file that lives entirely to serve one
// Snapshot call: Close both closes and removes it, so a caller that reads
// the snapshot and closes it leaves nothing behind on disk.
type tempFileReadCloser struct {
	*os.File
	path string
}

func openTempSnapshot(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("open snapshot: %w", err)
	}
	return &tempFileReadCloser{File: f, path: path}, nil
}

func (t *tempFileReadCloser) Close() error {
	closeErr := t.File.Close()
	removeErr := os.Remove(t.path)
	if closeErr != nil {
		return closeErr
	}
	if removeErr != nil && !os.IsNotExist(removeErr) {
		return removeErr
	}
	return nil
}
