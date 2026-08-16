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
	"github.com/evandelacruz/farrier/internal/core/restore"
)

// runRestore implements the `restore` command (RSTR-001): it connects to a
// fresh target over SSH and calls restore.Restore — the same function
// POST /restore calls — printing the same job event stream a dashboard
// would render over SSE.
func runRestore(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	bundleDir := fs.String("bundle", "", "path to the bundle directory (required)")
	target := fs.String("target", "", "ssh://user@host[:port] of the fresh restore target (required)")
	from := fs.String("from", "", "snapshot source: an s3:// URI or a filesystem directory (required, the same shape `backup -to` writes to)")
	snapshot := fs.String("snapshot", "", "snapshot key to restore (default: the newest snapshot in -from)")
	remoteDir := fs.String("remote-dir", orchestrate.DefaultRemoteDir, "directory on the host to deploy into")
	workDir := fs.String("work-dir", "", "local scratch directory for the fetched and decrypted snapshot (default: a fresh temporary directory)")
	keyFile := fs.String("ssh-key", "", "SSH private key file (default: the operator's SSH agent)")
	knownHosts := fs.String("known-hosts", "", "known_hosts file (default: ~/.ssh/known_hosts)")
	timeout := fs.Duration("ssh-timeout", 0, "SSH dial and handshake timeout (default: orchestrate.DefaultTimeout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *bundleDir == "" || *target == "" || *from == "" {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "farrier: restore: -bundle, -target, and -from are required")
		return 2
	}

	b, err := bundle.Load(*bundleDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: restore: load bundle: %v\n", err)
		return 1
	}

	dir := *workDir
	autoWorkDir := dir == ""
	if autoWorkDir {
		dir, err = os.MkdirTemp("", "farrier-restore-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "farrier: restore: create work directory: %v\n", err)
			return 1
		}
	}

	ctx := context.Background()
	opts, cleanup, err := prepareRestore(ctx, b, *target, *from, *snapshot, *remoteDir, dir, orchestrate.Options{
		KeyFile:        *keyFile,
		KnownHostsFile: *knownHosts,
		Timeout:        *timeout,
	})
	if err != nil {
		if autoWorkDir {
			os.RemoveAll(dir)
		}
		fmt.Fprintf(os.Stderr, "farrier: restore: %v\n", err)
		return 1
	}
	defer cleanup()

	job := events.NewJob()
	err = runJob(job, func() error {
		return restore.Restore(ctx, job, opts)
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: restore: %v\n", err)
		return 1
	}
	return 0
}

// prepareRestore resolves everything restore.Options needs beyond flags: it
// connects to target, builds the bundle's keystore and blob drivers,
// resolves the snapshot source and the bundle's age backup key, and
// assembles restore.Options the same way for both the CLI and API skins
// (API-001). The returned cleanup func closes the SSH connection; the
// caller must call it once done with opts.
func prepareRestore(ctx context.Context, b *bundle.Bundle, target, from, snapshotKey, remoteDir, workDir string, dialOpts orchestrate.Options) (restore.Options, func(), error) {
	client, err := orchestrate.Connect(ctx, target, dialOpts)
	if err != nil {
		return restore.Options{}, nil, fmt.Errorf("connect to %s: %w", target, err)
	}
	cleanup := func() { client.Close() }

	keystoreDriver, err := keystore.New(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		cleanup()
		return restore.Options{}, nil, fmt.Errorf("build keystore driver: %w", err)
	}

	blobAdapter, err := blob.New(b.Manifest.Drivers.Blob.Driver, b.Manifest.Drivers.Blob.Config)
	if err != nil {
		cleanup()
		return restore.Options{}, nil, fmt.Errorf("build blob driver: %w", err)
	}

	source, err := backup.OpenDestination(from)
	if err != nil {
		cleanup()
		return restore.Options{}, nil, fmt.Errorf("open snapshot source: %w", err)
	}

	identity, err := backup.ResolveIdentity(ctx, keystoreDriver)
	if err != nil {
		cleanup()
		return restore.Options{}, nil, err
	}

	opts := restore.Options{
		RemoteDir:   remoteDir,
		WorkDir:     workDir,
		Bundle:      b,
		Source:      source,
		SnapshotKey: snapshotKey,
		Identity:    identity,
		Keystore:    keystoreDriver,
		Blobs:       blobAdapter,
		Host:        client,
	}
	return opts, cleanup, nil
}
