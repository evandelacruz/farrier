package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/promote"
)

// runPromote implements the `promote` command (FAIL-001): it connects to a
// fresh standby target over SSH and calls promote.Promote — the same
// function POST /promote calls — printing the same job event stream a
// dashboard would render over SSE.
func runPromote(args []string) int {
	fs := flag.NewFlagSet("promote", flag.ContinueOnError)
	bundleDir := fs.String("bundle", "", "path to the bundle directory (required)")
	target := fs.String("target", "", "ssh://user@host[:port] of the fresh standby target (required)")
	from := fs.String("from", "", "snapshot source: an s3:// URI or a filesystem directory (required, the same shape `backup -to` writes to)")
	snapshot := fs.String("snapshot", "", "snapshot key to restore (default: the newest snapshot in -from)")
	remoteDir := fs.String("remote-dir", orchestrate.DefaultRemoteDir, "directory on the host to deploy into")
	workDir := fs.String("work-dir", "", "local scratch directory for the fetched and decrypted snapshot (default: a fresh temporary directory)")
	keyFile := fs.String("ssh-key", "", "SSH private key file (default: the operator's SSH agent)")
	knownHosts := fs.String("known-hosts", "", "known_hosts file (default: ~/.ssh/known_hosts)")
	timeout := fs.Duration("ssh-timeout", 0, "SSH dial and handshake timeout (default: orchestrate.DefaultTimeout)")
	dnsRecord := fs.String("dns-record", "", "DNS record to flip (default: the bundle's own domain)")
	dnsValue := fs.String("dns-value", "", "address (IP or hostname) to point dns-record at (default: -target's host)")
	yes := fs.Bool("yes", false, "skip the interactive confirmation prompt (for scripted/automated use); the snapshot age is still printed")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *bundleDir == "" || *target == "" || *from == "" {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "farrier: promote: -bundle, -target, and -from are required")
		return 2
	}

	b, err := bundle.Load(*bundleDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: promote: load bundle: %v\n", err)
		return 1
	}

	value := *dnsValue
	if value == "" {
		parsed, err := orchestrate.ParseTarget(*target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "farrier: promote: %v\n", err)
			return 1
		}
		value = parsed.Host
	}

	ctx := context.Background()
	resolvedKey, confirmed := confirmPromote(ctx, os.Stdin, os.Stdout, *from, *snapshot, *yes)
	if !confirmed {
		fmt.Fprintln(os.Stderr, "farrier: promote: aborted (not confirmed)")
		return 1
	}

	dir := *workDir
	autoWorkDir := dir == ""
	if autoWorkDir {
		dir, err = os.MkdirTemp("", "farrier-promote-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "farrier: promote: create work directory: %v\n", err)
			return 1
		}
	}

	job := events.NewJob()
	opts, cleanup, err := preparePromote(ctx, job, b, *target, *from, resolvedKey, *remoteDir, dir, *dnsRecord, value, orchestrate.Options{
		KeyFile:        *keyFile,
		KnownHostsFile: *knownHosts,
		Timeout:        *timeout,
	})
	if err != nil {
		if autoWorkDir {
			os.RemoveAll(dir)
		}
		fmt.Fprintf(os.Stderr, "farrier: promote: %v\n", err)
		return 1
	}
	defer cleanup()

	err = runJob(job, func() error {
		return promote.Promote(ctx, job, opts)
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: promote: %v\n", err)
		return 1
	}
	return 0
}

// preparePromote resolves everything promote.Options needs beyond flags:
// it connects to target, builds the bundle's keystore, blob, and DNS
// drivers, resolves the snapshot source and the bundle's age backup key,
// and assembles promote.Options the same way for both the CLI and API
// skins (API-001). The returned cleanup func closes the SSH connection;
// the caller must call it once done with opts. job is the same job the
// caller later runs promote.Promote against — required so a DNS driver
// resolved to dns.NewPrint(job) (DNS-003) reports onto the operation's own
// event stream rather than a throwaway one.
func preparePromote(ctx context.Context, job *events.Job, b *bundle.Bundle, target, from, snapshotKey, remoteDir, workDir, dnsRecord, dnsValue string, dialOpts orchestrate.Options) (promote.Options, func(), error) {
	client, err := orchestrate.Connect(ctx, target, dialOpts)
	if err != nil {
		return promote.Options{}, nil, fmt.Errorf("connect to %s: %w", target, err)
	}
	cleanup := func() { client.Close() }

	keystoreDriver, err := keystore.New(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		cleanup()
		return promote.Options{}, nil, fmt.Errorf("build keystore driver: %w", err)
	}

	blobAdapter, err := blob.New(b.Manifest.Drivers.Blob.Driver, b.Manifest.Drivers.Blob.Config)
	if err != nil {
		cleanup()
		return promote.Options{}, nil, fmt.Errorf("build blob driver: %w", err)
	}

	dnsDriver, err := promote.ResolveDNSDriver(ctx, job, b.Manifest.Drivers.DNS, keystoreDriver)
	if err != nil {
		cleanup()
		return promote.Options{}, nil, fmt.Errorf("build dns driver: %w", err)
	}

	source, err := backup.OpenDestination(from)
	if err != nil {
		cleanup()
		return promote.Options{}, nil, fmt.Errorf("open snapshot source: %w", err)
	}

	identity, err := backup.ResolveIdentity(ctx, keystoreDriver)
	if err != nil {
		cleanup()
		return promote.Options{}, nil, err
	}

	opts := promote.Options{
		RemoteDir:   remoteDir,
		WorkDir:     workDir,
		Bundle:      b,
		Source:      source,
		SnapshotKey: snapshotKey,
		Identity:    identity,
		Keystore:    keystoreDriver,
		Blobs:       blobAdapter,
		Host:        client,
		DNS:         dnsDriver,
		DNSRecord:   dnsRecord,
		DNSValue:    dnsValue,
	}
	return opts, cleanup, nil
}

// confirmPromote implements FAIL-002: it opens the snapshot source, resolves
// the age of the snapshot -snapshot names (or the newest one, if -snapshot
// is empty — the same resolution promote.Promote itself applies), prints it
// to out so the operator's decision is informed (spec.md "Failover": "the
// CLI makes the decision informed"), and returns the resolved snapshot key
// alongside whether the operator (or -yes, standing in for one in a script)
// confirmed proceeding. The caller must pass the returned key — not the raw
// -snapshot flag — into promote.Options.SnapshotKey: promote.Promote does
// its own "empty means newest" resolution, and re-running that resolution
// after the confirmation returns could promote a different snapshot than
// the one whose age was shown. skipPrompt bypasses the interactive wait but
// never the age display — the age is always shown, only the wait is
// optional.
func confirmPromote(ctx context.Context, in io.Reader, out io.Writer, from, snapshotKey string, skipPrompt bool) (string, bool) {
	source, err := backup.OpenDestination(from)
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: promote: open snapshot source: %v\n", err)
		return "", false
	}

	resolvedKey, age, err := backup.SnapshotAge(ctx, source, snapshotKey, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: promote: resolve snapshot: %v\n", err)
		return "", false
	}

	fmt.Fprintf(out, "promoting snapshot %s, captured %s ago\n", resolvedKey, age.Round(time.Second))

	if skipPrompt {
		return resolvedKey, true
	}

	fmt.Fprint(out, "this will restore this snapshot, verify it, start services, reconcile CI, and flip DNS. type \"yes\" to continue: ")
	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		return resolvedKey, false
	}
	return resolvedKey, strings.TrimSpace(strings.ToLower(scanner.Text())) == "yes"
}
