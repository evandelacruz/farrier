package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/evandelacruz/farrier/internal/core/acme"
	"github.com/evandelacruz/farrier/internal/core/attach"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// runAttach implements the `attach` command (UP-007): it connects to the
// host the instance runs on and calls attach.Attach, printing the same job
// event stream a dashboard would render over SSE.
//
// Nothing here decides anything. Whether the bundle may be named, whether
// the domain is well-formed, and whether the current address is spelled
// usably are all attach.Attach's calls, so the CLI and the API report the
// same refusals in the same words.
func runAttach(args []string) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	bundleDir := fs.String("bundle", "", "path to the nameless bundle's directory (required)")
	target := fs.String("target", "", "ssh://user@host[:port] of the host the instance runs on (required)")
	remoteDir := fs.String("remote-dir", orchestrate.DefaultRemoteDir, "directory on the host the instance was deployed into")
	domain := fs.String("domain", "", "the FQDN to attach (required)")
	dnsProvider := fs.String("acme-dns-provider", "", "lego DNS-01 provider name used to prove the zone (required)")
	email := fs.String("acme-email", "", "contact address registered on the ACME account")
	acmeDirectory := fs.String("acme-directory", "", "ACME directory URL to issue this instance's certificate against (default: Let's Encrypt production); pass "+acme.StagingShorthand+" for Let's Encrypt's staging environment, or a URL for an internal ACME CA. The choice is recorded in the bundle and every later renewal uses it")
	address := fs.String("address", "", "the address the instance is served at today, so the clone URLs it is changing from can be reported (required)")
	keyFile := fs.String("ssh-key", "", "SSH private key file (default: the operator's SSH agent)")
	knownHosts := fs.String("known-hosts", "", "known_hosts file (default: ~/.ssh/known_hosts)")
	timeout := fs.Duration("ssh-timeout", 0, "SSH dial and handshake timeout (default: orchestrate.DefaultTimeout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *bundleDir == "" || *target == "" {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "farrier: attach: -bundle and -target are required")
		return 2
	}

	b, err := bundle.Load(*bundleDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: attach: load bundle: %v\n", err)
		return 1
	}

	ctx := context.Background()
	client, err := orchestrate.Connect(ctx, *target, orchestrate.Options{
		KeyFile:        *keyFile,
		KnownHostsFile: *knownHosts,
		Timeout:        *timeout,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: attach: connect: %v\n", err)
		return 1
	}
	defer client.Close()

	job := events.NewJob()
	err = runJob(job, func() error {
		_, err := attach.Attach(ctx, job, attach.Options{
			BundleDir:       *bundleDir,
			RemoteDir:       *remoteDir,
			Bundle:          b,
			Host:            client,
			Domain:          *domain,
			ACMEDNSProvider: *dnsProvider,
			ACMEEmail:       *email,
			ACMEDirectory:   *acmeDirectory,
			Address:         *address,
		})
		return err
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: attach: %v\n", err)
		return 1
	}
	return 0
}
