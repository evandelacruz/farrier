// Package bundle defines the bundle directory format: the manifest and
// rendered Compose files that together describe a Farrier deployment.
//
// The manifest never carries key material. Driver config holds only
// references to where secrets live (a keystore driver name plus its
// non-secret config, e.g. a file path or a command line) — never a secret
// value itself. That, plus the fact that Bundle is loaded and saved purely
// from a directory path with no host-specific state retained, is what makes
// a bundle "function identically after being copied to another machine,
// given key access" (CORE-001).
package bundle

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultChecksumAlgorithm is the checksum algorithm new manifests use.
const DefaultChecksumAlgorithm = "sha256"

// StateKind identifies one of the four kinds of state a bundle declares.
type StateKind string

const (
	StateKindGit      StateKind = "git"
	StateKindDatabase StateKind = "database"
	StateKindBlobs    StateKind = "blobs"
	StateKindKeys     StateKind = "keys"
)

// AllStateKinds are the four kinds every bundle must declare exactly once.
var AllStateKinds = []StateKind{StateKindGit, StateKindDatabase, StateKindBlobs, StateKindKeys}

// StateDeclaration declares that a bundle carries one kind of state.
type StateDeclaration struct {
	Kind StateKind `yaml:"kind"`
}

// DriverRef points a manifest at a driver: its name, plus whatever non-secret
// config that driver needs to resolve or reach the thing it manages. For a
// keystore driver, Config is a pointer to a secret (a path, a command) —
// never the secret's value.
type DriverRef struct {
	Driver string         `yaml:"driver"`
	Config map[string]any `yaml:"config,omitempty"`
}

// DriverConfig is the manifest's driver section: keystore and blob are
// required, DNS is optional. DNS may be left unconfigured (DNS-003):
// operations then print the record change instead of applying it.
type DriverConfig struct {
	Keystore DriverRef `yaml:"keystore"`
	DNS      DriverRef `yaml:"dns,omitempty"`
	Blob     DriverRef `yaml:"blob"`
}

// Manifest is the bundle's farrier.yaml: domain, pinned image digests,
// driver config, state-kind declarations, and the checksum algorithm used
// throughout backup and restore.
type Manifest struct {
	Domain            string             `yaml:"domain"`
	Images            map[string]string  `yaml:"images"`
	Drivers           DriverConfig       `yaml:"drivers"`
	State             []StateDeclaration `yaml:"state"`
	ChecksumAlgorithm string             `yaml:"checksumAlgorithm"`
}

// NewManifest builds a manifest with all four state kinds declared and the
// default checksum algorithm set.
func NewManifest(domain string, images map[string]string, drivers DriverConfig) *Manifest {
	state := make([]StateDeclaration, len(AllStateKinds))
	for i, kind := range AllStateKinds {
		state[i] = StateDeclaration{Kind: kind}
	}
	return &Manifest{
		Domain:            domain,
		Images:            images,
		Drivers:           drivers,
		State:             state,
		ChecksumAlgorithm: DefaultChecksumAlgorithm,
	}
}

var digestPinned = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// Validate checks that a manifest is complete and internally consistent. It
// does not, and cannot, check that no secret ended up in Config — that's a
// property of the driver code that populates DriverRef, not of the manifest
// shape.
func (m *Manifest) Validate() error {
	if strings.TrimSpace(m.Domain) == "" {
		return fmt.Errorf("bundle: domain is required")
	}
	if len(m.Images) == 0 {
		return fmt.Errorf("bundle: at least one image is required")
	}
	for component, ref := range m.Images {
		if !digestPinned.MatchString(ref) {
			return fmt.Errorf("bundle: image %q must be pinned by digest (@sha256:...), got %q", component, ref)
		}
	}
	if strings.TrimSpace(m.Drivers.Keystore.Driver) == "" {
		return fmt.Errorf("bundle: keystore driver is required")
	}
	if strings.TrimSpace(m.Drivers.Blob.Driver) == "" {
		return fmt.Errorf("bundle: blob driver is required")
	}
	if err := validateState(m.State); err != nil {
		return err
	}
	if strings.TrimSpace(m.ChecksumAlgorithm) == "" {
		return fmt.Errorf("bundle: checksum algorithm is required")
	}
	return nil
}

func validateState(declared []StateDeclaration) error {
	seen := make(map[StateKind]bool, len(AllStateKinds))
	for _, d := range declared {
		if seen[d.Kind] {
			return fmt.Errorf("bundle: state kind %q declared more than once", d.Kind)
		}
		seen[d.Kind] = true
	}
	for _, kind := range AllStateKinds {
		if !seen[kind] {
			return fmt.Errorf("bundle: state kind %q must be declared", kind)
		}
	}
	if len(declared) != len(AllStateKinds) {
		return fmt.Errorf("bundle: state must declare exactly the four kinds, got %d", len(declared))
	}
	return nil
}
