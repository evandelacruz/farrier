package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/status"
)

// runStatus implements the `status` command (STAT-001): it connects to the
// target over SSH and calls status.Check, printing the resulting report.
// status is a read: its exit code reflects whether the report could be
// produced, not whether the instance it describes is healthy — an operator
// reads the printed report for that, the same way `docker ps` exits 0
// whether or not the containers it lists are up.
func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	bundleDir := fs.String("bundle", "", "path to the bundle directory (required)")
	target := fs.String("target", "", "ssh://user@host[:port] of the forge host (required)")
	remoteDir := fs.String("remote-dir", "/opt/farrier", "directory on the host farrier was deployed into")
	diskPath := fs.String("disk-path", status.DefaultDiskPath, "filesystem path on the host to report disk headroom for")
	keyFile := fs.String("ssh-key", "", "SSH private key file (default: the operator's SSH agent)")
	knownHosts := fs.String("known-hosts", "", "known_hosts file (default: ~/.ssh/known_hosts)")
	timeout := fs.Duration("ssh-timeout", 0, "SSH dial and handshake timeout (default: orchestrate.DefaultTimeout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *bundleDir == "" || *target == "" {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "farrier: status: -bundle and -target are required")
		return 2
	}

	b, err := bundle.Load(*bundleDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: status: load bundle: %v\n", err)
		return 1
	}

	driver, err := keystore.New(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: status: build keystore driver: %v\n", err)
		return 1
	}

	ctx := context.Background()
	client, err := orchestrate.Connect(ctx, *target, orchestrate.Options{
		KeyFile:        *keyFile,
		KnownHostsFile: *knownHosts,
		Timeout:        *timeout,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: status: connect: %v\n", err)
		return 1
	}
	defer client.Close()

	report, err := status.Check(ctx, status.Options{
		Runner:    client,
		Bundle:    b,
		RemoteDir: *remoteDir,
		Keystore:  driver,
		DiskPath:  *diskPath,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: status: %v\n", err)
		return 1
	}

	printReport(report)
	return 0
}

func printReport(r status.Report) {
	fmt.Println("services:")
	for _, s := range r.Services {
		state := "down"
		if s.Up {
			state = "up"
		}
		fmt.Printf("  %s: %s (%s)\n", s.Name, state, s.Detail)
	}

	fmt.Println("tls:")
	fmt.Printf("  expires: %s\n", r.TLS.NotAfter.Format(time.RFC3339))
	switch {
	case !r.TLS.Valid:
		fmt.Println("  status: invalid or expired")
	case r.TLS.ExpiringSoon:
		fmt.Printf("  status: valid, expiring soon (within %s)\n", status.CertExpiryWarning)
	default:
		fmt.Println("  status: valid")
	}

	fmt.Printf("disk (%s):\n", r.Disk.Path)
	fmt.Printf("  available: %s of %s\n", formatBytes(r.Disk.AvailableBytes), formatBytes(r.Disk.TotalBytes))
}

// formatBytes renders n as a human-readable size, e.g. "42.1 GB". Display
// formatting only — status.DiskStatus itself carries exact byte counts.
func formatBytes(n uint64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	units := "KMGTPE"
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), units[exp])
}
