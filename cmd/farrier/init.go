package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/initialize"
)

func runInit(args []string) int {
	params, code := parseInitFlags(args)
	if code != 0 {
		return code
	}

	job := events.NewJob()
	err := runJob(job, func() error {
		_, runErr := initialize.Run(context.Background(), job, params)
		return runErr
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: init: %v\n", err)
		return 1
	}
	return 0
}

// parseInitFlags turns init's flags into initialize.Params, returning a
// nonzero exit code (and having reported why on stderr) when the operator's
// invocation is unusable. Split out from runInit so the flag surface — in
// particular that -project defaults to the working directory and an unset
// -dir leaves the bundle location to the core — is testable without
// standing up a real ACME exchange.
func parseInitFlags(args []string) (initialize.Params, int) {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	domain := fs.String("domain", "", "the bundle's DNS name; omit it for a nameless bundle served over plain HTTP, with no zone to prove and nothing to own")
	project := fs.String("project", ".", "the project folder to make into a forge definition")
	dir := fs.String("dir", "", "directory to write the bundle to (default: "+bundle.DirName+" inside the project folder)")
	keystoreDriver := fs.String("keystore-driver", "", "keystore driver name, e.g. file or command (required)")
	blobDriver := fs.String("blob-driver", "", "blob driver name, e.g. local or s3 (required)")
	acmeDNSProvider := fs.String("acme-dns-provider", "", "lego DNS-01 provider name for zone-control proof, e.g. cloudflare or rfc2136 (required with -domain); reads that provider's credentials from the environment")
	acmeEmail := fs.String("acme-email", "", "contact email for the ACME account used to prove zone control")
	gitSSHPort := fs.Int("git-ssh-port", bundle.DefaultGitSSHPort, "host port the instance serves git over SSH on; 22 gives bare git@domain:owner/repo.git clone URLs, but the host's own sshd usually owns it")
	colocatedRunner := fs.Bool("colocated-runner", true, "deploy a Forgejo Actions runner on the forge host; false keeps CI off the machine holding git data and the database, and the operator registers a remote runner instead")
	var keystoreConfig, blobConfig, images keyValueFlag
	fs.Var(&keystoreConfig, "keystore-config", "keystore driver config as key=value (repeatable)")
	fs.Var(&blobConfig, "blob-config", "blob driver config as key=value (repeatable)")
	fs.Var(&images, "image", "image override as component=reference (repeatable); unset components use the built-in default tag, resolved to a digest like any other reference")

	if err := fs.Parse(args); err != nil {
		return initialize.Params{}, 2
	}

	if strings.TrimSpace(*project) == "" {
		fmt.Fprintln(os.Stderr, "farrier: init: -project cannot be empty")
		return initialize.Params{}, 2
	}
	if strings.TrimSpace(*keystoreDriver) == "" {
		fmt.Fprintln(os.Stderr, "farrier: init: -keystore-driver is required")
		return initialize.Params{}, 2
	}
	if strings.TrimSpace(*blobDriver) == "" {
		fmt.Fprintln(os.Stderr, "farrier: init: -blob-driver is required")
		return initialize.Params{}, 2
	}
	// Required only alongside -domain: a nameless bundle proves no zone and
	// issues no certificate, so it has no use for a DNS-01 provider
	// (INIT-005). The inverse — a provider with no domain — is a
	// contradiction the core rejects, with a message that says which of the
	// two the operator probably meant.
	if strings.TrimSpace(*domain) != "" && strings.TrimSpace(*acmeDNSProvider) == "" {
		fmt.Fprintln(os.Stderr, "farrier: init: -acme-dns-provider is required with -domain")
		return initialize.Params{}, 2
	}
	if err := bundle.ValidateGitSSHPort(*gitSSHPort); err != nil {
		fmt.Fprintf(os.Stderr, "farrier: init: -git-ssh-port: %v\n", err)
		return initialize.Params{}, 2
	}

	return initialize.Params{
		Domain:          *domain,
		Project:         *project,
		Dir:             *dir,
		Keystore:        bundle.DriverRef{Driver: *keystoreDriver, Config: keystoreConfig.asAny()},
		Blob:            bundle.DriverRef{Driver: *blobDriver, Config: blobConfig.asAny()},
		ACMEDNSProvider: *acmeDNSProvider,
		ACMEEmail:       *acmeEmail,
		Images:          images.asStrings(),
		ColocatedRunner: colocatedRunner,
		GitSSHPort:      *gitSSHPort,
	}, 0
}

// keyValueFlag collects repeated "key=value" flag occurrences into an
// ordered list, so init can turn -keystore-config/-blob-config into a driver
// config map and -image into a component-to-reference override map with the
// same flag.Value plumbing.
type keyValueFlag []string

func (f *keyValueFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *keyValueFlag) Set(value string) error {
	if !strings.Contains(value, "=") {
		return fmt.Errorf("expected key=value, got %q", value)
	}
	*f = append(*f, value)
	return nil
}

func (f keyValueFlag) asAny() map[string]any {
	if len(f) == 0 {
		return nil
	}
	out := make(map[string]any, len(f))
	for _, kv := range f {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

func (f keyValueFlag) asStrings() map[string]string {
	if len(f) == 0 {
		return nil
	}
	out := make(map[string]string, len(f))
	for _, kv := range f {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}
