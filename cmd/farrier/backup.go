package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// runBackup implements the `backup` command (BKUP-006): it connects to the
// target over SSH and calls backup.Backup — the same function POST /backup
// calls — printing the same job event stream a dashboard would render over
// SSE.
func runBackup(args []string) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	bundleDir := fs.String("bundle", "", "path to the bundle directory (required)")
	target := fs.String("target", "", "ssh://user@host[:port] of the forge host (required)")
	to := fs.String("to", "", "backup destination: an s3:// URI or a filesystem directory (required, spec.md \"Golden path\")")
	remoteDir := fs.String("remote-dir", "/opt/farrier", "directory on the host farrier was deployed into")
	workDir := fs.String("work-dir", "", "local scratch directory for the plain and encrypted snapshot (default: a fresh temporary directory)")
	keyFile := fs.String("ssh-key", "", "SSH private key file (default: the operator's SSH agent)")
	knownHosts := fs.String("known-hosts", "", "known_hosts file (default: ~/.ssh/known_hosts)")
	timeout := fs.Duration("ssh-timeout", 0, "SSH dial and handshake timeout (default: orchestrate.DefaultTimeout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *bundleDir == "" || *target == "" || *to == "" {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "farrier: backup: -bundle, -target, and -to are required")
		return 2
	}

	b, err := bundle.Load(*bundleDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: backup: load bundle: %v\n", err)
		return 1
	}

	dir := *workDir
	autoWorkDir := dir == ""
	if autoWorkDir {
		dir, err = os.MkdirTemp("", "farrier-backup-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "farrier: backup: create work directory: %v\n", err)
			return 1
		}
	}

	ctx := context.Background()
	opts, cleanup, err := prepareBackup(ctx, b, *target, *to, *remoteDir, dir, orchestrate.Options{
		KeyFile:        *keyFile,
		KnownHostsFile: *knownHosts,
		Timeout:        *timeout,
	})
	if err != nil {
		if autoWorkDir {
			os.RemoveAll(dir)
		}
		fmt.Fprintf(os.Stderr, "farrier: backup: %v\n", err)
		return 1
	}
	defer cleanup()

	job := events.NewJob()
	err = runJob(job, func() error {
		return backup.Backup(ctx, job, opts)
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: backup: %v\n", err)
		return 1
	}
	return 0
}

// prepareBackup resolves everything backup.Options needs beyond flags: it
// connects to target, builds the bundle's keystore and blob drivers, and
// hands them to backup.BuildOptions, which wires the SSH-backed state
// exporters and push hold the same way for both the CLI and API skins
// (API-001). The returned cleanup func closes the SSH connection; the
// caller must call it once done with opts.
func prepareBackup(ctx context.Context, b *bundle.Bundle, target, destination, remoteDir, workDir string, dialOpts orchestrate.Options) (backup.Options, func(), error) {
	client, err := orchestrate.Connect(ctx, target, dialOpts)
	if err != nil {
		return backup.Options{}, nil, fmt.Errorf("connect to %s: %w", target, err)
	}
	cleanup := func() { client.Close() }

	keystoreDriver, err := keystore.New(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		cleanup()
		return backup.Options{}, nil, fmt.Errorf("build keystore driver: %w", err)
	}

	blobAdapter, err := blob.New(b.Manifest.Drivers.Blob.Driver, b.Manifest.Drivers.Blob.Config)
	if err != nil {
		cleanup()
		return backup.Options{}, nil, fmt.Errorf("build blob driver: %w", err)
	}

	identity, err := backup.ResolveIdentity(ctx, keystoreDriver)
	if err != nil {
		cleanup()
		return backup.Options{}, nil, err
	}

	opts := backup.BuildOptions(client, b, remoteDir, workDir, destination, identity, blobAdapter, keystoreDriver)
	return opts, cleanup, nil
}
