package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/drill"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// runDrill implements the `drill` command (DRIL-001): it connects to a
// scratch target over SSH and calls drill.Drill — the same function
// POST /drill calls — printing the same job event stream a dashboard would
// render over SSE. The stream's terminal event is the report: on failure it
// names the specific step that failed.
//
// There is no -snapshot flag by design: a drill rehearses the most recent
// backup, which is the one an emergency would actually reach for.
func runDrill(args []string) int {
	fs := flag.NewFlagSet("drill", flag.ContinueOnError)
	bundleDir := fs.String("bundle", "", "path to the bundle directory (required)")
	target := fs.String("target", "", "ssh://user@host[:port] of the scratch drill target (required)")
	from := fs.String("from", "", "snapshot source: an s3:// URI or a filesystem directory (required, the same shape `backup -to` writes to)")
	remoteDir := fs.String("remote-dir", "/opt/farrier", "directory on the scratch target to deploy into")
	workDir := fs.String("work-dir", "", "local scratch directory for the fetched and decrypted snapshot (default: a fresh temporary directory)")
	keyFile := fs.String("ssh-key", "", "SSH private key file (default: the operator's SSH agent)")
	knownHosts := fs.String("known-hosts", "", "known_hosts file (default: ~/.ssh/known_hosts)")
	timeout := fs.Duration("ssh-timeout", 0, "SSH dial and handshake timeout (default: orchestrate.DefaultTimeout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *bundleDir == "" || *target == "" || *from == "" {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "farrier: drill: -bundle, -target, and -from are required")
		return 2
	}

	b, err := bundle.Load(*bundleDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: drill: load bundle: %v\n", err)
		return 1
	}

	dir := *workDir
	autoWorkDir := dir == ""
	if autoWorkDir {
		dir, err = os.MkdirTemp("", "farrier-drill-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "farrier: drill: create work directory: %v\n", err)
			return 1
		}
	}

	ctx := context.Background()
	opts, cleanup, err := prepareDrill(ctx, b, *target, *from, *remoteDir, dir, orchestrate.Options{
		KeyFile:        *keyFile,
		KnownHostsFile: *knownHosts,
		Timeout:        *timeout,
	})
	if err != nil {
		if autoWorkDir {
			os.RemoveAll(dir)
		}
		fmt.Fprintf(os.Stderr, "farrier: drill: %v\n", err)
		return 1
	}
	defer cleanup()

	job := events.NewJob()
	err = runJob(job, func() error {
		_, err := drill.Drill(ctx, job, opts)
		return err
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: drill: %v\n", err)
		return 1
	}
	return 0
}

// prepareDrill resolves everything drill.Options needs beyond flags: it
// connects to the scratch target, builds the bundle's keystore and blob
// drivers, resolves the snapshot source and the bundle's age backup key,
// and assembles drill.Options the same way for both the CLI and API skins
// (API-001). The returned cleanup func closes the SSH connection; the
// caller must call it once done with opts.
//
// No DNS driver is resolved here, unlike preparePromote: a drill never
// touches DNS.
func prepareDrill(ctx context.Context, b *bundle.Bundle, target, from, remoteDir, workDir string, dialOpts orchestrate.Options) (drill.Options, func(), error) {
	client, err := orchestrate.Connect(ctx, target, dialOpts)
	if err != nil {
		return drill.Options{}, nil, fmt.Errorf("connect to %s: %w", target, err)
	}
	cleanup := func() { client.Close() }

	keystoreDriver, err := keystore.New(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		cleanup()
		return drill.Options{}, nil, fmt.Errorf("build keystore driver: %w", err)
	}

	blobAdapter, err := blob.New(b.Manifest.Drivers.Blob.Driver, b.Manifest.Drivers.Blob.Config)
	if err != nil {
		cleanup()
		return drill.Options{}, nil, fmt.Errorf("build blob driver: %w", err)
	}

	source, err := backup.OpenDestination(from)
	if err != nil {
		cleanup()
		return drill.Options{}, nil, fmt.Errorf("open snapshot source: %w", err)
	}

	identity, err := backup.ResolveIdentity(ctx, keystoreDriver)
	if err != nil {
		cleanup()
		return drill.Options{}, nil, err
	}

	opts := drill.Options{
		RemoteDir: remoteDir,
		WorkDir:   workDir,
		Bundle:    b,
		Source:    source,
		Identity:  identity,
		Keystore:  keystoreDriver,
		Blobs:     blobAdapter,
		Host:      client,
	}
	return opts, cleanup, nil
}
