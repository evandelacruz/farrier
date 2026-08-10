// Package initialize implements INIT-001: building a bundle from a project
// folder, a DNS name, and a keystore target, written to bundle.DirName
// inside that folder unless the operator points somewhere else; INIT-002:
// proving control of that domain's DNS zone via an ACME DNS-01 challenge
// before the bundle is written; INIT-003: generating every piece of bundle
// key material and persisting it through the bundle's keystore driver;
// INIT-004: refusing to write over a bundle directory that already holds
// one, so re-running init in an initialized project cannot replace a live
// instance's identity; and INIT-005: accepting a project folder with no
// domain at all. It is the core logic behind the `init` CLI command —
// cmd/farrier's init command parses flags and calls Run; every real
// decision (validation, zone-control proof, key generation, image-digest
// resolution, manifest assembly) lives here so a future API frontend
// (API-001) can call the same function and get the same CORE-002 event
// stream.
//
// # Bundles without a name
//
// The domain is optional (spec.md "Instances without a name"). Leaving it
// empty produces a nameless bundle: Run skips zone-control proof and
// certificate issuance entirely, so the operator needs no domain, no DNS API
// token, and no TXT record to paste — the first minute costs nothing.
//
// Skipping is the only difference. Every other piece of key material is
// generated exactly as for a named bundle — SECRET_KEY, INTERNAL_TOKEN, the
// LFS JWT secret, the runner registration secret, the SSH host key, the age
// backup key — because a nameless instance is a complete instance in all
// respects but its name, and anything left ungenerated here would be missing
// from every backup the instance ever takes. The TLS certificate and its
// private key are the only two absent, and they are absent because there is
// no name to issue them for.
//
// A nameless bundle's manifest carries an empty domain and no ACME section,
// and Run rejects ACME inputs alongside an empty domain rather than
// accepting them and doing nothing with them: an operator who supplied a
// DNS-01 provider meant to supply a domain too, and silently producing a
// nameless bundle would hand them an instance with no HTTPS they did not ask
// for.
//
// Namelessness changes nothing about INIT-004: a nameless init refuses an
// already-initialized folder exactly as a named one does. The two compose —
// having no name is not a reason to be allowed to overwrite an identity.
//
// # A failed init can be re-run
//
// Storing key material is the one step init cannot undo, so the run is
// ordered and instrumented around it: everything fallible that persists
// nothing happens first, and a run that gets past the first store leaves a
// resume record the next init reads. See Run and resume.go.
package initialize

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
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
	StepRenderCompose    = "render-compose"
	StepWrite            = "write"
)

// DefaultImageRefs are the images init pins when the caller doesn't
// override a component: Forgejo itself, Caddy, and the Forgejo Actions
// runner — the stateless components every bundle needs (spec.md "What it's
// built on").
//
// These govern exactly one thing: what a fresh `init` picks up on day one.
// Run resolves each tag to a digest and writes the digest into the manifest
// (tech-spec.md "Bundle directory"), `up` deploys that digest unchanged, and
// `upgrade -image` takes an explicit reference rather than consulting these.
// A deployed bundle is frozen by design — nothing here ever moves it.
//
// Every default must be a tag that resolves. `latest` is not: Forgejo
// publishes no such tag, so defaulting to it made `init` fail at
// StepResolveImages with a registry 404 unless the operator passed an
// override — on the first command a new operator runs.
var DefaultImageRefs = map[string]string{
	// Forgejo's LTS line, supported to July 2027. Deliberately not 16: that
	// is the non-LTS current release, supported only to October 2026.
	"forgejo": "codeberg.org/forgejo/forgejo:15",
	// A fixed point release, matching what docker.io/library/caddy:latest
	// resolves to today.
	"caddy": "docker.io/library/caddy:2.11.4",
	// The runner's current stable major, and not coupled to the Forgejo pin:
	// the runner is forward-compatible across a wide range of Forgejo
	// versions, so the two lines version independently and their numbers do
	// not track each other. Forgejo 15 pairs with runner 13, which looks like
	// a mismatch and is not one — check code.forgejo.org rather than
	// inferring a number from the Forgejo pin.
	forge.RunnerService: "code.forgejo.org/forgejo/runner:13",
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
	// Domain is the bundle's DNS name (spec.md "The domain"). Empty is a
	// deliberate choice, not an omission: it produces a nameless bundle
	// (INIT-005), and ACMEDNSProvider and ACMEEmail must then be empty too.
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
	// Required with a Domain, and rejected without one.
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

	// WebPort is the host port `up` publishes the instance's web endpoint
	// on (UP-002, UP-006). Zero takes the default for the tier the bundle
	// is in — bundle.DefaultNamedWebPort with a domain,
	// bundle.DefaultNamelessWebPort without one — and an operator whose
	// host already serves something there sets their own. Like GitSSHPort,
	// the resolved value is written into the manifest explicitly.
	WebPort int

	// PublicWebPort is the port clients reach the instance on when
	// something on the host holds the standard port and forwards to
	// Farrier (bundle.Manifest.PublicWebPort). Zero means Caddy is the
	// edge, which is the ordinary case; unlike the two ports above it is
	// written into the manifest only when set, since a value that always
	// equals WebPort would suggest a knob where there is no choice.
	PublicWebPort int

	// Resolver resolves image refs to digests; nil uses registry.Resolve.
	Resolver Resolver
	// Prover proves ACME DNS-01 zone control; nil uses a real ACME exchange
	// (acmeProver).
	Prover Prover
}

// Run builds a bundle for params.Project and saves it to the bundle
// directory params resolves to — bundle.DirFor(Project) by default, or
// params.Dir when the operator names one — emitting CORE-002 progress
// events on job as it goes. It returns the bundle it wrote, or an error —
// with job carrying a StateFailed event either way, so a caller only needs
// to check the returned error, not separately inspect the event stream, to
// know whether init succeeded.
//
// # A failed init can always be re-run
//
// Storing key material is the one thing init does that it cannot take
// back: key material is non-rotating by design (spec.md "Key material"),
// so a run that stores some of it and then fails cannot clean up after
// itself without acquiring the ability to delete a live instance's
// identity. Run is ordered and instrumented so that never leaves the
// operator stuck.
//
// Everything fallible that persists nothing runs first — validation, image
// resolution, manifest assembly and Compose rendering, then zone-control
// proof — so the ordinary failures (a typo'd image, an unreachable
// registry, a DNS provider that will not answer) all happen while the
// keystore is still untouched and a retry is simply a retry. That ordering
// is what the defect this fixes needed: image resolution failed after
// seven pieces of key material had already been stored.
//
// What ordering cannot fix is a failure after the first Store: the
// keystore driver refusing halfway through, the bundle write failing, the
// operator pressing Ctrl-C, a panic. For those, Run writes a resume record
// into the bundle directory immediately before the first Store
// (resume.go). The next init reads it, finds the key material it names,
// and keeps that material as the instance's identity instead of colliding
// with it — so the recovery is `farrier init` again, with no file to
// delete by hand and no flag to remember. The record names only keys and a
// keystore fingerprint, so key material stays out of it (KEY-003), and it
// is removed the moment the bundle is on disk.
//
// The record is deliberately never removed on failure, cancellation, or
// panic. It is not cleanup — it is the evidence the retry needs, and the
// dangerous version of this fix is the one that deletes key material it
// cannot prove it wrote.
func Run(ctx context.Context, job *events.Job, params Params) (b *bundle.Bundle, err error) {
	named := strings.TrimSpace(params.Domain) != ""

	job.Started(StepValidate, "checking the project folder, domain, and driver targets")
	if err := validateName(params); err != nil {
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
	// The ACME DNS-01 provider is checked in validateName, not here: it is
	// required alongside a domain and refused without one (INIT-005).
	if err := bundle.ValidateGitSSHPort(params.GitSSHPort); err != nil {
		return fail(job, StepValidate, fmt.Errorf("initialize: %w", err))
	}
	// Both web ports, and the rule that ties them together: a named bundle
	// published somewhere other than the standard port has to say what
	// clients connect to. Checked here, before any key material is
	// generated, rather than when the assembled manifest is validated on
	// its way to disk.
	probe := bundle.Manifest{
		Domain:        strings.TrimSpace(params.Domain),
		WebPort:       params.WebPort,
		PublicWebPort: params.PublicWebPort,
	}
	if err := probe.ValidateWebPorts(); err != nil {
		return fail(job, StepValidate, fmt.Errorf("initialize: %w", err))
	}
	keystoreWriter, ok := keystoreDriver.(keystore.Writer)
	if !ok {
		return fail(job, StepValidate, fmt.Errorf("initialize: keystore driver %q cannot store generated key material; give the command driver a storeCommand, declare store: true on an out-of-tree driver that implements it, use the file driver, or provision Forgejo's key material manually first", params.Keystore.Driver))
	}
	// Asking the keystore what it already holds belongs here, with the
	// other refusals: a target that holds another instance's identity is
	// refused before an ACME exchange is spent, and one holding this
	// bundle's own unfinished work is claimed before anything is
	// generated for it.
	found, err := inspectKeystore(ctx, keystoreDriver, params.Keystore.Driver, dir, params.Keystore)
	if err != nil {
		return fail(job, StepValidate, err)
	}
	if named {
		job.Emit(StepValidate, events.StateSucceeded, fmt.Sprintf("project folder %s is ready; domain and driver targets are valid", params.Project))
	} else {
		job.Emit(StepValidate, events.StateSucceeded, fmt.Sprintf("project folder %s is ready; driver targets are valid, and no domain was given", params.Project))
	}
	for _, note := range found.Notes {
		job.Emit(StepValidate, events.StateSucceeded, note)
	}

	job.Started(StepResolveImages, "resolving image references to digests")
	images, err := resolveImages(ctx, resolverOrDefault(params.Resolver), params.Images)
	if err != nil {
		return fail(job, StepResolveImages, err)
	}
	job.Emit(StepResolveImages, events.StateSucceeded, fmt.Sprintf("resolved %d image(s)", len(images)))

	job.Started(StepRenderCompose, "assembling the manifest and rendering Compose")
	manifest := buildManifest(params, images, named)
	compose, err := orchestrate.Render(manifest)
	if err != nil {
		return fail(job, StepRenderCompose, fmt.Errorf("initialize: %w", err))
	}
	bundleToWrite := &bundle.Bundle{Manifest: *manifest, Compose: compose}
	job.Emit(StepRenderCompose, events.StateSucceeded, fmt.Sprintf("rendered %d Compose file(s)", len(compose)))

	// A nameless bundle has no zone to prove and no certificate to issue
	// (INIT-005), so the step reports what it skipped and why instead of
	// vanishing from the stream — an operator watching `init` should see
	// that the proof was skipped by design, not wonder whether it ran.
	var cert *acme.Certificate
	if named {
		job.Started(StepProveZoneControl, fmt.Sprintf("proving control of %s via ACME DNS-01", params.Domain))
		cert, err = proverOrDefault(params.Prover).Prove(params.Domain, params.ACMEDNSProvider, params.ACMEEmail)
		if err != nil {
			return fail(job, StepProveZoneControl, fmt.Errorf("initialize: prove control of %s: %w", params.Domain, err))
		}
		if cert == nil || len(cert.Certificate) == 0 || len(cert.PrivateKey) == 0 {
			return fail(job, StepProveZoneControl, fmt.Errorf("initialize: no certificate from zone-control proof of %s to persist", params.Domain))
		}
		job.Emit(StepProveZoneControl, events.StateSucceeded, fmt.Sprintf("zone control proven for %s", params.Domain))
	} else {
		job.Started(StepProveZoneControl, "no domain given; nothing to prove")
		job.Emit(StepProveZoneControl, events.StateSucceeded, "nameless bundle: skipping DNS-01 proof and certificate issuance; the forge is served over plain HTTP at an address given to `up`, and a domain can be attached later")
	}

	job.Started(StepGenerateKeys, "generating and storing bundle key material")
	material, err := generateKeyMaterial(cert, found.Reuse)
	if err != nil {
		return fail(job, StepGenerateKeys, err)
	}
	for name, secret := range found.Derived {
		material[name] = secret
	}

	// From here on the run can leave key material behind, so from here on
	// it is recoverable. The record goes down before the first Store, not
	// after, because the failure it has to survive is a process that stops
	// existing between two stores.
	if err := writeIncompleteRecord(dir, incompleteRecord{
		Schema:              incompleteSchema,
		Note:                incompleteNote,
		KeystoreDriver:      strings.TrimSpace(params.Keystore.Driver),
		KeystoreFingerprint: found.Fingerprint,
		Keys:                recordedKeys(material, found.Reuse),
	}); err != nil {
		return fail(job, StepGenerateKeys, err)
	}
	// A deferred call, the same shape drill's teardown uses (DRIL-003), so
	// the promise holds on every exit and not just the ones with a return
	// statement: a returned error, a canceled context, and a panic
	// unwinding from any depth all reach it.
	defer func() { announceIncomplete(job, b, err, dir) }()

	if err := storeKeyMaterial(ctx, keystoreWriter, material); err != nil {
		return fail(job, StepGenerateKeys, withRecovery(err, dir))
	}
	job.Emit(StepGenerateKeys, events.StateSucceeded, storedSummary(len(material), len(found.Reuse)))

	// INIT-006. It runs the moment the material is safely stored rather
	// than at the end of Run: a later step failing must not be the reason
	// an operator never learns where the age backup key went, since by
	// this point it exists and is already the one thing they cannot
	// re-derive.
	reportKeyMaterial(job, params.Keystore.Driver, keystoreDriver, material, found.Reuse)

	job.Started(StepWrite, "writing the bundle")
	// The manifest carries the host key's public half, so publishing a
	// project to this instance can pin its identity without holding the
	// keystore. It is filled in here rather than in buildManifest because
	// the key does not exist until the step above stored it — and Compose
	// does not derive from it, so nothing has to be re-rendered.
	if err := recordHostPublicKey(ctx, keystoreDriver, &bundleToWrite.Manifest); err != nil {
		return fail(job, StepWrite, withRecovery(err, dir))
	}
	if err := bundleToWrite.Save(dir); err != nil {
		return fail(job, StepWrite, withRecovery(fmt.Errorf("initialize: %w", err), dir))
	}
	// The bundle is on disk, so the run is no longer partial and the
	// record has nothing left to describe. A record that will not delete
	// is reported and not fatal: INIT-004 now refuses this folder anyway,
	// so the stale file can mislead nobody into a second init.
	if err := removeIncompleteRecord(dir); err != nil {
		job.Emit(StepWrite, events.StateSucceeded, fmt.Sprintf("the bundle is written, but %v; the file is safe to delete", err))
	}
	b = bundleToWrite
	job.Emit(StepWrite, events.StateSucceeded, fmt.Sprintf("bundle written to %s", dir))

	if named {
		job.Succeeded(fmt.Sprintf("bundle for %s created at %s, serving %s", strings.TrimSpace(params.Domain), dir, params.Project))
	} else {
		job.Succeeded(fmt.Sprintf("nameless bundle created at %s, serving %s; give `up` an address to serve it at, and attach a domain when the instance outlives the experiment", dir, params.Project))
	}
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

// withRecovery appends the retry instruction to a failure that happened
// after key material was written. Callers use it for exactly those
// failures: the operator's next move differs from a clean failure's, and
// an error that does not say so is the defect this package is fixing.
func withRecovery(err error, dir string) error {
	return fmt.Errorf("%w. %s", err, recoveryHint(dir))
}

// announceIncomplete is what Run defers once key material can exist: it
// makes sure a run that ends without a bundle has said so, and said what
// to do about it.
//
// On success and on a returned error it does nothing — fail() has already
// put the reason (with the recovery, via withRecovery) on the stream and
// closed it, and events.Job panics on an emit after a terminal event. The
// case it exists for is the one with no return value at all: a panic
// unwinding through Run leaves the job with no terminal event and the
// operator with key material in a keystore and no idea it is there.
func announceIncomplete(job *events.Job, b *bundle.Bundle, err error, dir string) {
	if (b != nil && err == nil) || job.Done() {
		return
	}
	job.Failed(fmt.Sprintf("init did not finish: %s", recoveryHint(dir)))
}

// buildManifest assembles the bundle manifest from params and the
// resolved image digests. Split out of Run so the manifest — the last
// thing that can fail before key material is written — is assembled and
// rendered while the keystore is still untouched.
func buildManifest(params Params, images map[string]string, named bool) *bundle.Manifest {
	// A nameless bundle's ACME section stays zero: nothing about it ever
	// reaches ACME, and Manifest.Validate rejects a manifest whose domain
	// and ACME section disagree.
	acmeConfig := bundle.ACMEConfig{}
	if named {
		acmeConfig = bundle.ACMEConfig{
			DNSProvider: params.ACMEDNSProvider,
			Email:       params.ACMEEmail,
		}
	}
	manifest := bundle.NewManifest(strings.TrimSpace(params.Domain), images, bundle.DriverConfig{
		Keystore: params.Keystore,
		Blob:     params.Blob,
	}, acmeConfig)
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
	// And again for the web port, which is the one an operator is most
	// likely to have to change: the host is theirs and may already be
	// serving something on 443 or 8222. Written at the tier's default so
	// farrier.yaml names the port the instance is published on rather than
	// leaving the operator to infer it (UP-002, UP-006).
	manifest.WebPort = params.WebPort
	if manifest.WebPort == 0 {
		manifest.WebPort = manifest.WebPortOrDefault()
	}
	// Not written at a default: an unset public port means Caddy is the
	// edge, and spelling that as a number equal to WebPort would read as a
	// second endpoint rather than as the absence of one.
	manifest.PublicWebPort = params.PublicWebPort
	return manifest
}

// recordedKeys lists the non-rotating key material this instance's
// identity is made of: what this run is about to store plus what an
// earlier run already stored. Rotating material (the TLS pair) is left
// out — a retry reissues it and the keystore accepts the overwrite, so
// recording it would claim a hold the record does not need.
func recordedKeys(material map[string]keystore.Secret, reuse map[string]bool) []string {
	names := make([]string, 0, len(material)+len(reuse))
	for _, name := range identityKeys() {
		if _, ok := material[name]; ok || reuse[name] {
			names = append(names, name)
		}
	}
	return names
}

// storedSummary is the generate-keys step's closing line. It counts reused
// material separately so a resumed init reads as a resumed init rather
// than as one that mysteriously stored fewer keys than the last attempt.
func storedSummary(stored, reused int) string {
	if reused == 0 {
		return fmt.Sprintf("stored %d piece(s) of key material", stored)
	}
	return fmt.Sprintf("stored %d piece(s) of key material and kept %d from an earlier unfinished init", stored, reused)
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

// validateName checks the domain and the ACME inputs against each other.
//
// No domain is legal — it is what asks for a nameless bundle (INIT-005) —
// but only on its own. ACME inputs without a domain are refused rather than
// ignored: there is nothing to prove and nothing to issue a certificate for,
// so an operator who named a DNS-01 provider intended a named bundle and
// left the domain out by mistake. Accepting it would hand them a nameless
// instance, and the mistake would surface later as a forge with no HTTPS.
func validateName(params Params) error {
	domain := strings.TrimSpace(params.Domain)
	provider := strings.TrimSpace(params.ACMEDNSProvider)

	if domain == "" {
		if provider != "" || strings.TrimSpace(params.ACMEEmail) != "" {
			return fmt.Errorf("initialize: acme dns-01 settings were given without a domain; pass a domain to prove its zone, or drop them for a nameless bundle")
		}
		return nil
	}
	// The grammar lives on bundle, beside the field it validates, so the
	// name `init` accepts and the name `attach` accepts (UP-007) are the
	// same set.
	if err := bundle.ValidateDomain(domain); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if provider == "" {
		return fmt.Errorf("initialize: acme dns-01 provider is required to prove control of %s", domain)
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
// for a named bundle, zone-control proof sits in between and takes as long
// as a DNS record takes to propagate. Two inits racing for the same folder
// is not the case
// INIT-004 is about — a person re-running init in a project they already
// initialized is — and closing that window would mean holding a lock
// across an unbounded network operation.
func refuseExistingBundle(dir string) error {
	exists, err := bundle.Exists(dir)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if !exists {
		return nil
	}
	// A resume record beside the bundle means a Save got part-way — the
	// torn-bundle case bundle.Exists deliberately counts as existing. The
	// refusal stands, but "remove that folder" is the wrong instruction
	// there: the record is the only thing that lets a later init reuse
	// the key material already in the keystore, so it has to outlive the
	// manifest and compose/ the operator clears out.
	if _, statErr := os.Stat(filepath.Join(dir, IncompleteFile)); statErr == nil {
		return fmt.Errorf("initialize: %s holds part of a bundle from an init that did not finish, and refusing to write over a bundle directory is not something init will guess its way past. Remove %s and %s from that folder — but keep %s, which is what lets the next init reuse the key material already in your keystore instead of colliding with it — then re-run init",
			dir, bundle.ManifestFile, bundle.ComposeDir, IncompleteFile)
	}
	return fmt.Errorf("initialize: %s already holds a bundle; refusing to overwrite it, because a second init would replace the instance's identity with newly generated key material. Remove that folder deliberately, or give init another location, to create a second bundle", dir)
}
