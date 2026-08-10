// Package initialize implements INIT-001: building a bundle from a project
// folder, a DNS name, and a keystore target, written to bundle.DirName
// inside that folder unless the operator points somewhere else; INIT-002:
// proving control of that domain's DNS zone via an ACME DNS-01 challenge
// before the bundle is written; INIT-003: generating every piece of bundle
// key material and persisting it through the bundle's keystore driver; and
// INIT-004: refusing to write over a bundle directory that already holds
// one, so re-running init in an initialized project cannot replace a live
// instance's identity. It is the core logic behind the
// `init` CLI command — cmd/farrier's init command parses flags and calls
// Run; every real decision (validation, zone-control proof, key generation,
// image-digest resolution, manifest assembly) lives here so a future API
// frontend (API-001) can call the same function and get the same CORE-002
// event stream.
package initialize

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/acme"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/registry"
)

// Step names emitted through the job's event stream (CORE-002).
const (
	StepValidate         = "validate"
	StepProveZoneControl = "prove-zone-control"
	StepGenerateKeys     = "generate-keys"
	StepReportKeys       = "report-key-material"
	StepResolveImages    = "resolve-images"
	StepWrite            = "write"
)

// DefaultImageRefs are the images init pins when the caller doesn't
// override a component: Forgejo itself, Caddy, and the Forgejo Actions
// runner — the stateless components every bundle needs (spec.md "What it's
// built on"). Each is a tag, not yet a digest — Run resolves it through the
// registry package, so the manifest always ends up digest-pinned
// (tech-spec.md "Bundle directory") even though the default here is a
// floating tag.
var DefaultImageRefs = map[string]string{
	"forgejo":           "codeberg.org/forgejo/forgejo:latest",
	"caddy":             "docker.io/library/caddy:latest",
	forge.RunnerService: "code.forgejo.org/forgejo/runner:latest",
}

// requiredComponents are the images a bundle cannot function without:
// forge.Service ("forgejo") is the container FORGE-002's admin bootstrap
// and every future forge operation target by name, caddy is the bundle's
// sole TLS terminator (spec.md "What it's built on"), and the runner is what
// makes a workflow pushed to a fresh deployment actually run (FORGE-005).
// Run refuses to write a bundle missing any of them, default or overridden.
//
// The runner is required even when Params.ColocatedRunner is false: the
// manifest is shareable and long-lived, so pinning the image an operator
// would need to turn the runner back on costs nothing and means re-enabling
// it is a one-line edit rather than a registry lookup.
var requiredComponents = []string{"forgejo", "caddy", forge.RunnerService}

// Resolver resolves an image reference to its digest-pinned form. Satisfied
// by registry.Resolver; declared here so Run is testable without real
// network calls.
type Resolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// Prover proves control of domain's DNS zone via an ACME DNS-01 challenge
// (INIT-002), returning the reason it couldn't when proof fails, or the
// certificate the exchange obtained when it succeeds — INIT-003's job is to
// persist that certificate as bundle key material rather than let it go to
// waste. Satisfied by acmeProver, which backs it with a real ACME DNS-01
// exchange (acme.Issue, ACME-001); declared here so Run is testable without
// a real ACME server or DNS provider.
type Prover interface {
	Prove(domain, dnsProvider, email string) (*acme.Certificate, error)
}

// acmeProver is the production Prover: it runs a full ACME DNS-01 exchange
// via acme.Issue, using an account key generated fresh for the proof. A
// successful exchange both proves zone control (INIT-002) and produces the
// certificate INIT-003 persists — one exchange serves both, rather than
// proving control and then issuing a second certificate for the same
// domain against the same provider.
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

// Params are init's inputs: the project folder, DNS name, and keystore
// target INIT-001 requires, plus the blob target and image references
// Manifest.Validate also requires before a bundle can be saved
// (bundle/manifest.go). Overriding Images is optional — any component left
// unset falls back to DefaultImageRefs — but Keystore and Blob have no
// default: both point at operator infrastructure Run cannot guess.
type Params struct {
	// Domain is the bundle's DNS name (spec.md "The domain").
	Domain string

	// Project is the project folder the forge is being stood up for. It
	// must already exist: `init` turns a folder that holds code into a
	// forge definition, so a path that isn't there is a typo rather than a
	// folder to create.
	Project string
	// Dir overrides where the bundle is written. Empty — the shape the
	// design optimizes for — writes it to bundle.DirFor(Project), so the
	// forge definition is versioned with the code it serves. An explicit
	// Dir is for the one bundle that belongs to no single project: an
	// instance serving several of them, where making one project own the
	// forge that hosts the other nine would be arbitrary (spec.md "The
	// unit: one forge per project"). A location, not a mode — nothing else
	// about the bundle differs.
	Dir string

	// ACMEDNSProvider is the lego-recognized DNS-01 provider name (e.g.
	// "cloudflare", "rfc2136") Run proves zone control through (INIT-002).
	// It is independent of Keystore and of the manifest's own DNS driver
	// (bundle.DriverConfig.DNS): lego resolves the named provider from its
	// own provider set and reads that provider's credentials from the
	// process environment, the way the operator already runs any lego-based
	// tool — Run neither reads nor sets them.
	ACMEDNSProvider string
	// ACMEEmail is the optional contact address registered on the ACME
	// account Run creates to perform the proof.
	ACMEEmail string

	// Keystore is the "keystore target": driver name plus its non-secret
	// config, exactly as the manifest's DriverConfig.Keystore carries it.
	Keystore bundle.DriverRef
	// Blob is the blob adapter's driver target, required by
	// Manifest.Validate the same way Keystore is.
	Blob bundle.DriverRef

	// Images overrides DefaultImageRefs per component. A component absent
	// here uses its default; present, it replaces the default entirely.
	// Every ref, default or override, may be a tag or an exact digest —
	// Run resolves either through Resolver.
	Images map[string]string

	// ColocatedRunner declares whether the bundle deploys its Actions
	// runner on the forge host (FORGE-005). It defaults to true, and an
	// operator sets it false to keep CI off the machine holding git data
	// and the database — see bundle.ActionsConfig.ColocatedRunner and
	// spec.md "CI trust boundary". Either way the choice is written into
	// the manifest explicitly, so it is visible and reversible without
	// re-running init.
	ColocatedRunner *bool

	// GitSSHPort is the host port the instance serves git over SSH on
	// (UP-005). Zero takes bundle.DefaultGitSSHPort; an operator whose host
	// sshd does not own 22 sets it to 22 and gets bare
	// `git@domain:owner/repo.git` clone URLs. Like ColocatedRunner, the
	// resolved value is written into the manifest explicitly, so
	// farrier.yaml shows the knob and changing it later is an edit plus a
	// re-run of `up` rather than a re-run of init.
	GitSSHPort int

	// Resolver resolves image refs to digests; nil uses registry.Resolve.
	Resolver Resolver
	// Prover proves ACME DNS-01 zone control; nil uses a real ACME exchange
	// (acmeProver).
	Prover Prover
}

var domainPattern = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`)

// Run builds a bundle for params.Project and saves it to the bundle
// directory params resolves to — bundle.DirFor(Project) by default, or
// params.Dir when the operator names one — emitting CORE-002 progress
// events on job as it goes. It returns the bundle it wrote, or an error —
// with job carrying a StateFailed event either way, so a caller only needs
// to check the returned error, not separately inspect the event stream, to
// know whether init succeeded.
func Run(ctx context.Context, job *events.Job, params Params) (*bundle.Bundle, error) {
	job.Started(StepValidate, "checking the project folder, domain, and driver targets")
	if err := validateDomain(params.Domain); err != nil {
		return fail(job, StepValidate, err)
	}
	if err := validateProject(params.Project); err != nil {
		return fail(job, StepValidate, err)
	}
	dir := BundleDir(params)
	if err := refuseExistingBundle(dir); err != nil {
		return fail(job, StepValidate, err)
	}
	keystoreDriver, err := keystore.New(params.Keystore.Driver, params.Keystore.Config)
	if err != nil {
		return fail(job, StepValidate, fmt.Errorf("initialize: keystore target: %w", err))
	}
	if strings.TrimSpace(params.Blob.Driver) == "" {
		return fail(job, StepValidate, fmt.Errorf("initialize: blob driver is required"))
	}
	if strings.TrimSpace(params.ACMEDNSProvider) == "" {
		return fail(job, StepValidate, fmt.Errorf("initialize: acme dns-01 provider is required"))
	}
	if err := bundle.ValidateGitSSHPort(params.GitSSHPort); err != nil {
		return fail(job, StepValidate, fmt.Errorf("initialize: %w", err))
	}
	keystoreWriter, ok := keystoreDriver.(keystore.Writer)
	if !ok {
		return fail(job, StepValidate, fmt.Errorf("initialize: keystore driver %q cannot store generated key material; give the command driver a storeCommand, declare store: true on an out-of-tree driver that implements it, use the file driver, or provision Forgejo's key material manually first", params.Keystore.Driver))
	}
	job.Emit(StepValidate, events.StateSucceeded, fmt.Sprintf("project folder %s is ready; domain and driver targets are valid", params.Project))

	job.Started(StepProveZoneControl, fmt.Sprintf("proving control of %s via ACME DNS-01", params.Domain))
	cert, err := proverOrDefault(params.Prover).Prove(params.Domain, params.ACMEDNSProvider, params.ACMEEmail)
	if err != nil {
		return fail(job, StepProveZoneControl, fmt.Errorf("initialize: prove control of %s: %w", params.Domain, err))
	}
	job.Emit(StepProveZoneControl, events.StateSucceeded, fmt.Sprintf("zone control proven for %s", params.Domain))

	job.Started(StepGenerateKeys, "generating and storing bundle key material")
	material, err := generateKeyMaterial(cert)
	if err != nil {
		return fail(job, StepGenerateKeys, err)
	}
	if err := storeKeyMaterial(ctx, keystoreWriter, material); err != nil {
		return fail(job, StepGenerateKeys, err)
	}
	job.Emit(StepGenerateKeys, events.StateSucceeded, fmt.Sprintf("stored %d piece(s) of key material", len(material)))

	// INIT-006. It runs the moment the material is safely stored rather
	// than at the end of Run: a later step failing must not be the reason
	// an operator never learns where the age backup key went, since by
	// this point it exists and is already the one thing they cannot
	// re-derive.
	reportKeyMaterial(job, params.Keystore.Driver, keystoreDriver, material)

	job.Started(StepResolveImages, "resolving image references to digests")
	images, err := resolveImages(ctx, resolverOrDefault(params.Resolver), params.Images)
	if err != nil {
		return fail(job, StepResolveImages, err)
	}
	job.Emit(StepResolveImages, events.StateSucceeded, fmt.Sprintf("resolved %d image(s)", len(images)))

	job.Started(StepWrite, "rendering compose and writing the bundle")
	manifest := bundle.NewManifest(params.Domain, images, bundle.DriverConfig{
		Keystore: params.Keystore,
		Blob:     params.Blob,
	}, bundle.ACMEConfig{
		DNSProvider: params.ACMEDNSProvider,
		Email:       params.ACMEEmail,
	})
	// Written out even when it matches the default, so farrier.yaml shows
	// the operator both that the colocated runner exists and how to turn it
	// off (FORGE-005, spec.md "CI trust boundary").
	colocatedRunner := params.ColocatedRunner == nil || *params.ColocatedRunner
	manifest.Actions.ColocatedRunner = &colocatedRunner
	// Same reason: written out even at its default so farrier.yaml shows
	// the operator which port clients reach git over SSH on, and that it is
	// theirs to change (UP-005, spec.md "Reaching the forge").
	manifest.GitSSHPort = params.GitSSHPort
	if manifest.GitSSHPort == 0 {
		manifest.GitSSHPort = bundle.DefaultGitSSHPort
	}
	compose, err := orchestrate.Render(manifest)
	if err != nil {
		return fail(job, StepWrite, fmt.Errorf("initialize: %w", err))
	}
	b := &bundle.Bundle{Manifest: *manifest, Compose: compose}
	if err := b.Save(dir); err != nil {
		return fail(job, StepWrite, fmt.Errorf("initialize: %w", err))
	}
	job.Emit(StepWrite, events.StateSucceeded, fmt.Sprintf("bundle written to %s", dir))

	job.Succeeded(fmt.Sprintf("bundle for %s created at %s, serving %s", params.Domain, dir, params.Project))
	return b, nil
}

// BundleDir reports where Run will write params' bundle: params.Dir when
// the operator named one, otherwise bundle.DirFor(params.Project). Exported
// so a frontend can tell the operator the path before the job runs, and so
// there is exactly one place the default lives.
func BundleDir(params Params) string {
	if dir := strings.TrimSpace(params.Dir); dir != "" {
		return dir
	}
	return bundle.DirFor(strings.TrimSpace(params.Project))
}

func fail(job *events.Job, step string, err error) (*bundle.Bundle, error) {
	job.Emit(step, events.StateFailed, err.Error())
	job.Failed(err.Error())
	return nil, err
}

func resolverOrDefault(r Resolver) Resolver {
	if r != nil {
		return r
	}
	return registry.Resolver{}
}

func proverOrDefault(p Prover) Prover {
	if p != nil {
		return p
	}
	return acmeProver{}
}

// resolveImages merges overrides onto DefaultImageRefs, checks every
// requiredComponents entry is present, and resolves each resulting
// reference to its digest-pinned form.
func resolveImages(ctx context.Context, r Resolver, overrides map[string]string) (map[string]string, error) {
	refs := make(map[string]string, len(DefaultImageRefs)+len(overrides))
	for component, ref := range DefaultImageRefs {
		refs[component] = ref
	}
	for component, ref := range overrides {
		if strings.TrimSpace(ref) == "" {
			return nil, fmt.Errorf("initialize: image %q: reference is required", component)
		}
		refs[component] = ref
	}

	for _, component := range requiredComponents {
		if _, ok := refs[component]; !ok {
			return nil, fmt.Errorf("initialize: image %q is required", component)
		}
	}

	resolved := make(map[string]string, len(refs))
	for component, ref := range refs {
		pinned, err := r.Resolve(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("initialize: image %q: %w", component, err)
		}
		resolved[component] = pinned
	}
	return resolved, nil
}

// validateProject checks the project folder exists and is a directory. The
// bundle is written inside it by default, so a path that is missing or is a
// regular file is caught here — before zone-control proof spends an ACME
// exchange and before key material is generated — rather than at the write
// step with the operator's inputs already half-consumed.
func validateProject(project string) error {
	p := strings.TrimSpace(project)
	if p == "" {
		return fmt.Errorf("initialize: project folder is required")
	}
	info, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("initialize: project folder %q does not exist", project)
		}
		return fmt.Errorf("initialize: project folder %q: %w", project, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("initialize: project folder %q is not a directory", project)
	}
	return nil
}

// refuseExistingBundle implements INIT-004: a bundle directory that
// already holds a bundle is never written over, and the error names the
// folder so the operator knows which one to move, remove, or point away
// from with an explicit location.
//
// It runs in the validate step, ahead of everything that costs something.
// Zone-control proof spends a real ACME exchange (INIT-002) and key
// generation mints an identity and persists it through the operator's
// keystore (INIT-003); a second `init` that is going to be refused should
// spend neither. Refusing at the write step instead would leave the
// keystore holding a second instance's worth of key material for an
// instance that was never created.
//
// The check is not, and cannot be, atomic with the write that follows it:
// zone-control proof sits in between and takes as long as a DNS record
// takes to propagate. Two inits racing for the same folder is not the case
// INIT-004 is about — a person re-running init in a project they already
// initialized is — and closing that window would mean holding a lock
// across an unbounded network operation.
func refuseExistingBundle(dir string) error {
	exists, err := bundle.Exists(dir)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if exists {
		return fmt.Errorf("initialize: %s already holds a bundle; refusing to overwrite it, because a second init would replace the instance's identity with newly generated key material. Remove that folder deliberately, or give init another location, to create a second bundle", dir)
	}
	return nil
}

func validateDomain(domain string) error {
	d := strings.TrimSpace(domain)
	if d == "" {
		return fmt.Errorf("initialize: domain is required")
	}
	if !domainPattern.MatchString(d) {
		return fmt.Errorf("initialize: domain %q is not a valid DNS name", domain)
	}
	return nil
}
