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
	"github.com/evandelacruz/farrier/internal/core/upgrade"
)

// runUpgrade implements the `upgrade` command (UPGR-001): it connects to
// the target over SSH and calls upgrade.Upgrade — the same function
// POST /upgrade calls — printing the same job event stream a dashboard
// would render over SSE.
func runUpgrade(args []string) int {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	bundleDir := fs.String("bundle", "", "path to the bundle directory (required)")
	target := fs.String("target", "", "ssh://user@host[:port] of the forge host (required)")
	to := fs.String("to", "", "pre-upgrade backup destination: an s3:// URI or a filesystem directory (required, the same shape `backup -to` takes)")
	image := fs.String("image", "", "forgejo image to upgrade to, a tag or an exact digest (required)")
	remoteDir := fs.String("remote-dir", "/opt/farrier", "directory on the host farrier was deployed into")
	workDir := fs.String("work-dir", "", "local scratch directory for the pre-upgrade backup (default: a fresh temporary directory)")
	diskPath := fs.String("disk-path", "", "filesystem path on the host to check disk headroom on (default: status.DefaultDiskPath)")
	keyFile := fs.String("ssh-key", "", "SSH private key file (default: the operator's SSH agent)")
	knownHosts := fs.String("known-hosts", "", "known_hosts file (default: ~/.ssh/known_hosts)")
	timeout := fs.Duration("ssh-timeout", 0, "SSH dial and handshake timeout (default: orchestrate.DefaultTimeout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *bundleDir == "" || *target == "" || *to == "" || *image == "" {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "farrier: upgrade: -bundle, -target, -to, and -image are required")
		return 2
	}

	b, err := bundle.Load(*bundleDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: upgrade: load bundle: %v\n", err)
		return 1
	}

	dir := *workDir
	autoWorkDir := dir == ""
	if autoWorkDir {
		dir, err = os.MkdirTemp("", "farrier-upgrade-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "farrier: upgrade: create work directory: %v\n", err)
			return 1
		}
	}

	ctx := context.Background()
	opts, cleanup, err := prepareUpgrade(ctx, b, *bundleDir, *target, *to, *image, *remoteDir, dir, *diskPath, orchestrate.Options{
		KeyFile:        *keyFile,
		KnownHostsFile: *knownHosts,
		Timeout:        *timeout,
	})
	if err != nil {
		if autoWorkDir {
			os.RemoveAll(dir)
		}
		fmt.Fprintf(os.Stderr, "farrier: upgrade: %v\n", err)
		return 1
	}
	defer cleanup()

	job := events.NewJob()
	err = runJob(job, func() error {
		return upgrade.Upgrade(ctx, job, opts)
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: upgrade: %v\n", err)
		return 1
	}
	return 0
}

// prepareUpgrade resolves everything upgrade.Options needs beyond flags: it
// connects to target, builds the bundle's keystore and blob drivers, and
// resolves the bundle's age backup key, assembling upgrade.Options the same
// way for both the CLI and API skins (API-001). The returned cleanup func
// closes the SSH connection; the caller must call it once done with opts.
func prepareUpgrade(ctx context.Context, b *bundle.Bundle, bundleDir, target, destination, newImage, remoteDir, workDir, diskPath string, dialOpts orchestrate.Options) (upgrade.Options, func(), error) {
	client, err := orchestrate.Connect(ctx, target, dialOpts)
	if err != nil {
		return upgrade.Options{}, nil, fmt.Errorf("connect to %s: %w", target, err)
	}
	cleanup := func() { client.Close() }

	keystoreDriver, err := keystore.New(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		cleanup()
		return upgrade.Options{}, nil, fmt.Errorf("build keystore driver: %w", err)
	}

	blobAdapter, err := blob.New(b.Manifest.Drivers.Blob.Driver, b.Manifest.Drivers.Blob.Config)
	if err != nil {
		cleanup()
		return upgrade.Options{}, nil, fmt.Errorf("build blob driver: %w", err)
	}

	identity, err := backup.ResolveIdentity(ctx, keystoreDriver)
	if err != nil {
		cleanup()
		return upgrade.Options{}, nil, err
	}

	opts := upgrade.Options{
		BundleDir:   bundleDir,
		RemoteDir:   remoteDir,
		WorkDir:     workDir,
		Bundle:      b,
		Destination: destination,
		NewImage:    newImage,
		Identity:    identity,
		Keystore:    keystoreDriver,
		Blobs:       blobAdapter,
		Host:        client,
		DiskPath:    diskPath,
	}
	return opts, cleanup, nil
}
