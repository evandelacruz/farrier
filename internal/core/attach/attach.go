// Package attach implements UP-007: giving a running nameless instance a
// name, in place. It proves control of the new domain's zone and issues a
// certificate (ACME-001, the same exchange `init` performs for a named
// bundle), persists that certificate as bundle key material, writes the
// domain into the manifest, re-renders every piece of configuration that
// derives from it, and reports the clone URLs that changed (CORE-002).
//
// # Why in place rather than a fresh init and up
//
// Everything an operator would lose in a rebuild — repositories, history,
// pull requests, review comments, CI history, secrets — is host state under
// RemoteDir/state (UP-004) or key material in the keystore, and the SSH
// host key is bundle key material that never rotates (RSTR-004). None of it
// derives from the name. Only the name derives from the name, so only the
// name changes: spec.md "Instances without a name" promises exactly this,
// and Attach is the operation that keeps the promise.
//
// Attach owns none of the machinery it sequences. Certificate issuance is
// acme.Issue's, config rendering and convergence are deploy.Up's, and the
// SSH host key is reinstalled by the same deploy.configureSSHHostKey that
// installs it on every ordinary `up`. Attach's own work is four things:
// deciding the operation is legal, obtaining and persisting the one piece
// of key material a nameless bundle lacks, rewriting the manifest, and
// telling the operator which URLs their team has to re-point.
//
// # Order, and what a failure leaves behind
//
// The certificate is obtained and persisted before the manifest is written,
// and the manifest is written before the host is touched. Each step is
// therefore only reached once the step that could still leave the operator
// with nothing changed has succeeded:
//
//   - Zone proof fails: nothing has changed anywhere. The bundle is still
//     nameless and the instance is still serving plain HTTP at its address.
//   - Persisting the certificate fails: same, plus a certificate that was
//     issued and not kept. Re-running Attach issues another one — the TLS
//     certificate and its private key are the one piece of key material the
//     keystore's rotation guard permits overwriting (keystore.Rotates), so
//     the retry is not blocked by the partial write.
//   - The manifest write fails: the bundle is still nameless. Nothing on
//     the host has been touched.
//   - The deployment fails: the bundle on disk is named, the instance is
//     still serving its old endpoint, and the fix is `farrier up` against
//     the now-named bundle — which converges the host to exactly what
//     Attach was about to converge it to. Attach says so in its failure.
//     Re-running Attach itself would refuse, because the bundle it would
//     name already has a name; see nameless below.
//
// # Naming an instance that already has a name
//
// Refused, always. A named bundle's domain is its identity — clone URLs,
// webhooks, runner registration, OAuth callbacks, LFS endpoints all derive
// from it (spec.md "The domain") — and relocating a named instance is a DNS
// flip precisely so that no remote ever changes. Renaming one would break
// every remote, invalidate the certificate the bundle carries, and orphan
// the DNS record failover flips (FAIL-004), which is a different operation
// with different consequences than the one UP-007 describes. Attach is the
// nameless-to-named transition and nothing else; the refusal names the
// domain the bundle already carries so an operator who reached for the
// wrong command can see why.
package attach

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/acme"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// Step identifiers Attach itself emits through the job's event stream
// (CORE-002), in the order it runs them, beyond the ones deploy.Up relays
// through unchanged once it is called.
//
// None of them collides with a deploy.Up step: the re-render is reported
// under deploy's own StepConfigureForge, StepConfigureTLS, and StepConverge,
// which is what actually happened, and a second step here claiming to have
// re-rendered anything would double-report the same work.
const (
	StepValidate           = "validate"
	StepProveZoneControl   = "prove-zone-control"
	StepPersistCertificate = "persist-certificate"
	StepNameBundle         = "name-bundle"
	StepReportCloneURLs    = "report-clone-urls"
)

// Host is everything Attach needs from a connected SSH session — exactly
// deploy.Host, since deploy.Up performs every host-touching step.
type Host = deploy.Host

// Prover proves control of domain's DNS zone via an ACME DNS-01 challenge
// and returns the certificate that exchange obtained (ACME-001). It is the
// same contract initialize.Prover states, spelled here so this package does
// not depend on `init` to name a running instance — the two operations
// share an ACME exchange, not a lifecycle. Satisfied by acmeProver;
// declared as an interface so Attach is testable without a real ACME server
// or DNS provider.
type Prover interface {
	Prove(domain, dnsProvider, email string) (*acme.Certificate, error)
}

// acmeProver is the production Prover: a full ACME DNS-01 exchange through
// acme.Issue, using an account key generated fresh for the proof. One
// exchange both proves zone control and produces the certificate the next
// step persists, rather than proving control and then issuing a second
// certificate for the same domain — the same reason initialize's prover
// keeps its certificate.
//
// Registering an ACME account carries none of Let's Encrypt's issuance rate
// limits, so a throwaway account key per exchange is free; the certificate
// it obtains is what is kept.
type acmeProver struct{}

func (acmeProver) Prove(domain, dnsProvider, email string) (*acme.Certificate, error) {
	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate acme account key: %w", err)
	}
	return acme.Issue(acme.Config{
		Domain:      domain,
		Email:       email,
		AccountKey:  accountKey,
		DNSProvider: dnsProvider,
	})
}

func proverOrDefault(p Prover) Prover {
	if p != nil {
		return p
	}
	return acmeProver{}
}

// Options configures Attach.
type Options struct {
	// BundleDir is the local directory Bundle was loaded from. The named
	// manifest and its re-rendered Compose are saved back here, so every
	// later command — `up`, `status`, `backup`, `promote` — sees an
	// instance that has a name (CORE-001 bundle content). Required.
	BundleDir string

	// RemoteDir is the directory on the host the instance was deployed
	// into, and the one the re-render is shipped to. It must be the
	// directory the instance is already running from: that is where its
	// state lives (UP-004), and pointing somewhere else would deploy a
	// second, empty instance rather than rename this one. Required.
	RemoteDir string

	// Bundle is the nameless bundle to name, loaded from BundleDir.
	// Required, and required to be nameless — see the package doc.
	Bundle *bundle.Bundle

	// Host is the already-connected session to the host the instance runs
	// on. Attach does not dial it or close it, the same split deploy.Up
	// draws. Required.
	Host Host

	// Domain is the FQDN to attach. It must be a name the operator
	// controls in DNS, since proving that control is the first thing
	// Attach does. Required.
	Domain string

	// ACMEDNSProvider is the lego-recognized DNS-01 provider name (e.g.
	// "cloudflare", "rfc2136") the zone is proven through, and the one
	// written into the manifest so renewal reissues through the same
	// provider (ACME-002). Required — a named bundle with no provider is
	// one whose certificate expires with no way to replace it, which
	// Manifest.Validate refuses anyway.
	//
	// lego reads that provider's credentials from the process environment,
	// never from this field; Attach neither reads nor sets them.
	ACMEDNSProvider string

	// ACMEEmail is the optional contact address registered on the ACME
	// account, and is recorded in the manifest alongside the provider.
	ACMEEmail string

	// Address is the address the instance is currently served at — the one
	// the operator gave `up` (UP-006). Required, and used for exactly one
	// thing: the "was" half of the clone-URL report.
	//
	// It is an input rather than something Attach reads back off the host
	// because a nameless instance's address is deliberately not bundle
	// identity (deploy.Options.Address): nothing persists it, so there is
	// nowhere authoritative to read it from, and the operator is holding
	// it — it is the flag they typed to bring the instance up.
	Address string

	// Prover proves zone control and issues the certificate; nil runs a
	// real ACME DNS-01 exchange.
	Prover Prover

	// CertIssuer is passed through to deploy.Up unchanged; nil uses the
	// real ACME-backed issuer. By the time Up runs, the certificate this
	// operation just issued is already persisted, so the issuer finds a
	// fresh certificate and reaches no ACME server (deploy.configureTLS).
	CertIssuer deploy.CertIssuer
}

// Attach names opts.Bundle's instance opts.Domain, in place (UP-007), as
// one CORE-002 job, and returns the named bundle it wrote.
//
// It owns job's terminal event, calling job.Succeeded or job.Failed exactly
// once. See the package doc for the order of the steps and what each
// failure leaves behind.
func Attach(ctx context.Context, job *events.Job, opts Options) (*bundle.Bundle, error) {
	named, err := attach(ctx, job, opts)
	if err != nil {
		job.Failed(err.Error())
		return nil, err
	}
	job.Succeeded(fmt.Sprintf(
		"%s is now served at %s; repositories, history, pull requests, review comments, CI history, secrets, and the SSH host key are untouched — consumers re-point their remote once",
		opts.RemoteDir, strings.TrimSpace(opts.Domain),
	))
	return named, nil
}

func attach(ctx context.Context, job *events.Job, opts Options) (*bundle.Bundle, error) {
	job.Started(StepValidate, "checking the bundle, the domain, and the keystore target")
	checked, err := validate(ctx, opts)
	if err != nil {
		job.Emit(StepValidate, events.StateFailed, err.Error())
		return nil, err
	}
	job.Emit(StepValidate, events.StateSucceeded, fmt.Sprintf("nameless bundle at %s is ready to be named %s", opts.BundleDir, checked.domain))

	job.Started(StepProveZoneControl, fmt.Sprintf("proving control of %s via ACME DNS-01", checked.domain))
	cert, err := proverOrDefault(opts.Prover).Prove(checked.domain, checked.dnsProvider, strings.TrimSpace(opts.ACMEEmail))
	if err != nil {
		err = fmt.Errorf("attach: prove control of %s: %w", checked.domain, err)
		job.Emit(StepProveZoneControl, events.StateFailed, err.Error())
		return nil, err
	}
	if cert == nil || len(cert.Certificate) == 0 || len(cert.PrivateKey) == 0 {
		err := fmt.Errorf("attach: no certificate from zone-control proof of %s to persist", checked.domain)
		job.Emit(StepProveZoneControl, events.StateFailed, err.Error())
		return nil, err
	}
	job.Emit(StepProveZoneControl, events.StateSucceeded, fmt.Sprintf("zone control proven for %s and a certificate issued", checked.domain))

	job.Started(StepPersistCertificate, "storing the certificate as bundle key material")
	if err := persistCertificate(ctx, checked.keystore, cert); err != nil {
		job.Emit(StepPersistCertificate, events.StateFailed, err.Error())
		return nil, err
	}
	job.Emit(StepPersistCertificate, events.StateSucceeded, fmt.Sprintf("%s and %s stored through the %s keystore driver", state.KeyTLSCertificate, state.KeyTLSPrivateKey, opts.Bundle.Manifest.Drivers.Keystore.Driver))

	job.Started(StepNameBundle, fmt.Sprintf("writing %s into the manifest and re-rendering compose", checked.domain))
	named, err := namedBundle(opts.Bundle, checked.domain, bundle.ACMEConfig{
		DNSProvider: checked.dnsProvider,
		Email:       strings.TrimSpace(opts.ACMEEmail),
	})
	if err != nil {
		job.Emit(StepNameBundle, events.StateFailed, err.Error())
		return nil, err
	}
	if err := named.Save(opts.BundleDir); err != nil {
		err = fmt.Errorf("attach: save named manifest: %w", err)
		job.Emit(StepNameBundle, events.StateFailed, err.Error())
		return nil, err
	}
	job.Emit(StepNameBundle, events.StateSucceeded, fmt.Sprintf("bundle at %s is now named %s", opts.BundleDir, checked.domain))

	if err := runDeploy(ctx, job, named, opts); err != nil {
		return nil, err
	}

	reportCloneURLs(job, &named.Manifest, checked.address)
	return named, nil
}

// checkedOptions is what validate resolved: the trimmed domain and DNS-01
// provider, the address spelled the way `up` spelled it, and the keystore
// driver the certificate is persisted through.
type checkedOptions struct {
	domain      string
	dnsProvider string
	address     string
	keystore    keystore.Driver
}

// validate refuses every input Attach cannot act on, before anything is
// spent. Order matters within it: the cheap structural checks run first,
// then the keystore target is built and checked for writability, because
// the alternative is discovering the operator's keystore cannot store a
// certificate *after* an ACME exchange has already obtained one — the same
// reason initialize checks its keystore target before proving a zone.
func validate(ctx context.Context, opts Options) (checkedOptions, error) {
	var checked checkedOptions

	if opts.Bundle == nil {
		return checked, errors.New("attach: bundle is required")
	}
	if strings.TrimSpace(opts.BundleDir) == "" {
		return checked, errors.New("attach: bundle directory is required")
	}
	if strings.TrimSpace(opts.RemoteDir) == "" {
		return checked, errors.New("attach: remote directory is required")
	}
	if opts.Host == nil {
		return checked, errors.New("attach: host is required")
	}
	if opts.Bundle.Manifest.Named() {
		return checked, fmt.Errorf(
			"attach: this bundle is already named %s; a name is an instance's identity and every clone URL, webhook, and runner registration derives from it, so it is not renamed in place. Relocating a named instance is a DNS flip, and `up` converges it to its existing name",
			strings.TrimSpace(opts.Bundle.Manifest.Domain),
		)
	}

	domain := strings.TrimSpace(opts.Domain)
	if err := bundle.ValidateDomain(domain); err != nil {
		return checked, fmt.Errorf("attach: %w", err)
	}
	provider := strings.TrimSpace(opts.ACMEDNSProvider)
	if provider == "" {
		return checked, fmt.Errorf("attach: acme dns-01 provider is required to prove control of %s and to renew its certificate later (ACME-002)", domain)
	}

	if strings.TrimSpace(opts.Address) == "" {
		return checked, errors.New("attach: the address this instance is currently served at is required, so the clone URLs it is changing from can be reported (UP-007)")
	}
	address, err := deploy.NormalizeAddress(strings.TrimSpace(opts.Address))
	if err != nil {
		return checked, fmt.Errorf("attach: current address: %w", err)
	}

	driver, err := keystore.New(opts.Bundle.Manifest.Drivers.Keystore.Driver, opts.Bundle.Manifest.Drivers.Keystore.Config)
	if err != nil {
		return checked, fmt.Errorf("attach: keystore driver: %w", err)
	}
	if _, ok := driver.(keystore.Writer); !ok {
		return checked, fmt.Errorf("attach: keystore driver %q cannot store the certificate this attaches; a nameless bundle holds no TLS key material, so naming it has to write some", opts.Bundle.Manifest.Drivers.Keystore.Driver)
	}
	// A nameless bundle holds no TLS key material at all (INIT-005). Finding
	// some means this bundle is not the nameless bundle it claims to be —
	// a manifest that lost its domain to a bad edit, or a directory holding
	// two instances' material — and overwriting it would replace a live
	// instance's certificate with one issued for a different name.
	if err := refuseExistingCertificate(ctx, driver); err != nil {
		return checked, err
	}

	checked = checkedOptions{domain: domain, dnsProvider: provider, address: address, keystore: driver}
	return checked, nil
}

// refuseExistingCertificate fails when the keystore already holds TLS key
// material for this bundle.
//
// A resolve error that is not keystore.ErrNotFound is treated as a refusal
// too, never as "nothing there": the check itself failed, and falling
// through would overwrite key material on the strength of a permission
// error or a malformed exec-driver response — the same reasoning
// keystore's own rotation guard applies to non-rotating names.
func refuseExistingCertificate(ctx context.Context, driver keystore.Driver) error {
	switch _, err := driver.Resolve(ctx, state.KeyTLSCertificate); {
	case err == nil:
		return fmt.Errorf("attach: the keystore already holds %s, but this bundle's manifest carries no domain; refusing to overwrite a certificate that belongs to some other name. Reconcile the manifest and the keystore before naming this instance", state.KeyTLSCertificate)
	case !errors.Is(err, keystore.ErrNotFound):
		return fmt.Errorf("attach: could not confirm the keystore holds no certificate yet: %w", err)
	}
	return nil
}

// persistCertificate stores the issued certificate and its private key as
// bundle key material, under the names every other part of the system reads
// them by (state.KeyTLSCertificate/state.KeyTLSPrivateKey): deploy.Up
// resolves them to configure Caddy, `status` resolves them to check expiry,
// and `backup` captures them as part of the key state kind (STATE-004). A
// nameless bundle's backups carry neither, so this is also the moment its
// snapshots start carrying a certificate.
//
// Nothing about the certificate's bytes reaches the event stream, here or
// anywhere else in this package (KEY-003) — the caller reports the key
// names and the driver, never a value.
func persistCertificate(ctx context.Context, driver keystore.Driver, cert *acme.Certificate) error {
	writer, ok := driver.(keystore.Writer)
	if !ok {
		return errors.New("attach: keystore driver cannot store key material")
	}
	if err := writer.Store(ctx, state.KeyTLSCertificate, keystore.NewSecret(string(cert.Certificate))); err != nil {
		return fmt.Errorf("attach: store %s: %w", state.KeyTLSCertificate, err)
	}
	if err := writer.Store(ctx, state.KeyTLSPrivateKey, keystore.NewSecret(string(cert.PrivateKey))); err != nil {
		return fmt.Errorf("attach: store %s: %w", state.KeyTLSPrivateKey, err)
	}
	return nil
}

// namedBundle returns a copy of b carrying domain and acmeCfg, with Compose
// re-rendered from that manifest — deploy.Up ships b.Compose to the host
// as-is, so the manifest alone is not enough; Compose has to carry the name
// too (orchestrate.Render writes FARRIER_DOMAIN into the services it
// renders for a named bundle).
//
// b itself is left untouched, and nothing else about the manifest moves:
// the pinned image digests, the git-over-SSH port, the driver config, the
// runner setting, and the state declarations are all copied through
// unchanged. That is the whole of "in place" at the manifest level — the
// only field that differs between the bundle that went in and the bundle
// that comes out is the one the operator asked to change, plus the ACME
// section Manifest.Validate requires to accompany it.
func namedBundle(b *bundle.Bundle, domain string, acmeCfg bundle.ACMEConfig) (*bundle.Bundle, error) {
	manifest := b.Manifest
	manifest.Domain = domain
	manifest.ACME = acmeCfg

	compose, err := orchestrate.Render(&manifest)
	if err != nil {
		return nil, fmt.Errorf("attach: render compose for %s: %w", domain, err)
	}
	return &bundle.Bundle{Manifest: manifest, Compose: compose}, nil
}

// runDeploy re-renders every piece of configuration that derives from the
// name and converges the host to it, through deploy.Up on the now-named
// bundle — app.ini's ROOT_URL, DOMAIN, and SSH_DOMAIN, the Caddy site block
// (TLS this time, with the certificate persisted two steps ago), and the
// published port moving from 80 to 443. It relays every step event onto job
// as it happens, the same relay pattern promote.restoreOnto and
// upgrade.runDeploy use and for the same reason: deploy.Up ends whatever
// job it is given, so sharing job would close job's stream mid-run.
//
// No Address is passed. The bundle is named now, and deploy.serveAddress
// refuses an address alongside a name — which is the check confirming this
// deployment really is the named path rather than the nameless one
// re-running with a domain bolted on.
//
// Nothing here touches forge state. deploy.Up creates the same host state
// directories under RemoteDir/state it created the first time (idempotent),
// bind-mounts them into the same services, reinstalls the same persisted
// SSH host key (RSTR-004), and starts the same pinned Forgejo image against
// the same database — so the container is recreated and its contents are
// not. That is why UP-007 is a re-render rather than a rebuild.
func runDeploy(ctx context.Context, job *events.Job, named *bundle.Bundle, opts Options) error {
	deployJob := events.NewJob()
	stream, cancel := deployJob.Subscribe()
	defer cancel()

	relayed := make(chan struct{})
	go func() {
		defer close(relayed)
		for ev := range stream {
			if ev.Step == "" {
				continue
			}
			job.Emit(ev.Step, ev.State, ev.Detail)
		}
	}()

	err := deploy.Up(ctx, deployJob, opts.Host, named, deploy.Options{
		RemoteDir:  opts.RemoteDir,
		CertIssuer: opts.CertIssuer,
	})
	<-relayed
	if err != nil {
		// The bundle on disk is already named by this point, so the retry
		// is `up`, not a second `attach` — which would refuse, having been
		// handed a bundle that now has a name. Saying so here is the
		// difference between a recoverable failure and one an operator has
		// to reason their way out of.
		return fmt.Errorf("attach: re-render configuration for %s: %w; the bundle at %s is already named, so re-run `up` against it to finish converging the host", named.Manifest.Domain, err, opts.BundleDir)
	}
	return nil
}
