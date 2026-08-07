// Package initialize implements INIT-001: building a bundle from a DNS name
// and a keystore target. It is the core logic behind the `init` CLI command
// — cmd/farrier's init command parses flags and calls Run; every real
// decision (validation, image-digest resolution, manifest assembly) lives
// here so a future API frontend (API-001) can call the same function and
// get the same CORE-002 event stream.
//
// Zone-control proof (INIT-002) and key-material generation (INIT-003) are
// separate requirement IDs and land as later additions to this package —
// Run does not perform either yet, so the manifest it writes has no keys
// resolvable through its keystore driver until INIT-003 lands.
package initialize

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/registry"
)

// Step names emitted through the job's event stream (CORE-002).
const (
	StepValidate      = "validate"
	StepResolveImages = "resolve-images"
	StepWrite         = "write"
)

// DefaultImageRefs are the images init pins when the caller doesn't
// override a component: Forgejo itself and Caddy, the two stateless
// components every bundle needs (spec.md "What it's built on"). Each is a
// tag, not yet a digest — Run resolves it through the registry package, so
// the manifest always ends up digest-pinned (tech-spec.md "Bundle
// directory") even though the default here is a floating tag.
var DefaultImageRefs = map[string]string{
	"forgejo": "codeberg.org/forgejo/forgejo:latest",
	"caddy":   "docker.io/library/caddy:latest",
}

// requiredComponents are the images a bundle cannot function without:
// forge.Service ("forgejo") is the container FORGE-002's admin bootstrap
// and every future forge operation target by name, and caddy is the bundle's
// sole TLS terminator (spec.md "What it's built on"). Run refuses to write a
// bundle missing either, default or overridden.
var requiredComponents = []string{"forgejo", "caddy"}

// Resolver resolves an image reference to its digest-pinned form. Satisfied
// by registry.Resolver; declared here so Run is testable without real
// network calls.
type Resolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// Params are init's inputs: the DNS name and keystore target INIT-001
// requires, plus the blob target and image references Manifest.Validate
// also requires before a bundle can be saved (bundle/manifest.go).
// Overriding Images is optional — any component left unset falls back to
// DefaultImageRefs — but Keystore and Blob have no default: both point at
// operator infrastructure Run cannot guess.
type Params struct {
	// Domain is the bundle's DNS name (spec.md "The domain").
	Domain string
	// Dir is the directory Run writes the bundle to.
	Dir string

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

	// Resolver resolves image refs to digests; nil uses registry.Resolve.
	Resolver Resolver
}

var domainPattern = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`)

// Run builds a bundle from params and saves it to params.Dir, emitting
// CORE-002 progress events on job as it goes. It returns the bundle it
// wrote, or an error — with job carrying a StateFailed event either way, so
// a caller only needs to check the returned error, not separately inspect
// the event stream, to know whether init succeeded.
func Run(ctx context.Context, job *events.Job, params Params) (*bundle.Bundle, error) {
	job.Started(StepValidate, "checking domain and driver targets")
	if err := validateDomain(params.Domain); err != nil {
		return fail(job, StepValidate, err)
	}
	if strings.TrimSpace(params.Dir) == "" {
		return fail(job, StepValidate, fmt.Errorf("initialize: bundle directory is required"))
	}
	if _, err := keystore.New(params.Keystore.Driver, params.Keystore.Config); err != nil {
		return fail(job, StepValidate, fmt.Errorf("initialize: keystore target: %w", err))
	}
	if strings.TrimSpace(params.Blob.Driver) == "" {
		return fail(job, StepValidate, fmt.Errorf("initialize: blob driver is required"))
	}
	job.Emit(StepValidate, events.StateSucceeded, "domain and driver targets are valid")

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
	})
	compose, err := orchestrate.Render(manifest)
	if err != nil {
		return fail(job, StepWrite, fmt.Errorf("initialize: %w", err))
	}
	b := &bundle.Bundle{Manifest: *manifest, Compose: compose}
	if err := b.Save(params.Dir); err != nil {
		return fail(job, StepWrite, fmt.Errorf("initialize: %w", err))
	}
	job.Emit(StepWrite, events.StateSucceeded, fmt.Sprintf("bundle written to %s", params.Dir))

	job.Succeeded(fmt.Sprintf("bundle for %s created at %s", params.Domain, params.Dir))
	return b, nil
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
