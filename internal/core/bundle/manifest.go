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

// DefaultGitSSHPort is the host port `up` publishes the forge's
// git-over-SSH server on when a manifest names none (UP-005).
//
// 2222 rather than 22 because the host's own sshd normally owns 22:
// Farrier reaches hosts over it (ORCH-001), and taking it would mean asking
// the operator to reconfigure their host, which the design deliberately
// does not do (spec.md "Reaching the forge"). The cost is a port in the SSH
// clone URL, which Forgejo renders into the URLs it displays — an operator
// whose host sshd lives elsewhere sets 22 here and gets bare
// `git@domain:owner/repo.git` URLs back.
const DefaultGitSSHPort = 2222

// maxPort is the highest TCP port number a manifest may declare.
const maxPort = 65535

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

// ACMEConfig is the bundle's ACME DNS-01 configuration: the lego-recognized
// provider name `init` proved zone control through (INIT-002) and the same
// provider `up` (UP-002) and renewal (ACME-002) reissue certificates
// through. It is independent of DriverConfig.DNS, the bundle's own DNS
// driver for record management — lego resolves DNSProvider from its own
// provider set and reads that provider's credentials from the process
// environment, never from this config (acme.Config's doc comment).
//
// A nameless bundle (Manifest.Domain empty, INIT-005) leaves this section
// empty: there is no zone to prove and no certificate to reissue, so
// carrying a provider name would describe work nothing performs. Validate
// enforces that in both directions.
type ACMEConfig struct {
	DNSProvider string `yaml:"dnsProvider,omitempty"`
	Email       string `yaml:"email,omitempty"`
}

// isZero reports whether an ACME section carries nothing at all — the shape
// a nameless bundle's manifest has.
func (c ACMEConfig) isZero() bool {
	return strings.TrimSpace(c.DNSProvider) == "" && strings.TrimSpace(c.Email) == ""
}

// ActionsConfig is the manifest's CI section: what `up` deploys alongside
// the forge to run Forgejo Actions jobs.
type ActionsConfig struct {
	// ColocatedRunner declares whether `up` deploys the bundle's Actions
	// runner on the forge host itself (FORGE-005). It is a pointer because
	// unset and false mean different things: unset is the default and means
	// enabled, since a fresh deployment must be able to run a workflow
	// without the operator doing anything; false is a deliberate operator
	// choice.
	//
	// Setting it to false is the escape hatch spec.md "CI trust boundary" >
	// "The colocated runner holds the host's Docker socket" depends on: the
	// colocated runner can start any container on the forge host, so an
	// operator who does not want that risk on the machine holding git data
	// and the database turns it off here and registers a remote runner
	// against the bundle domain instead. Runner registrations live in the
	// Forgejo database either way, so nothing else about the instance
	// changes (FAIL-005).
	ColocatedRunner *bool `yaml:"colocatedRunner,omitempty"`
}

// Manifest is the bundle's farrier.yaml: domain, git-over-SSH host port,
// pinned image digests, driver config, ACME DNS-01 config, CI runner
// config, state-kind declarations, and the checksum algorithm used
// throughout backup and restore.
type Manifest struct {
	// Domain is the DNS name the instance's identity derives from
	// (spec.md "The domain"). It is optional: an absent domain is what
	// makes a bundle nameless (INIT-005), and namelessness is the absence
	// of a name rather than a separate flag, so there is one field to read
	// and nothing that can disagree with itself. A nameless bundle carries
	// no certificate and no ACME section; `up` serves it over plain HTTP at
	// an address the operator supplies (UP-006), and attaching a domain
	// later fills this field in place (UP-007).
	Domain string `yaml:"domain,omitempty"`

	// GitSSHPort is the host port `up` publishes the forge's git-over-SSH
	// server on, and the port Forgejo advertises in the SSH clone URLs it
	// displays (UP-005). Zero means unset and resolves to
	// DefaultGitSSHPort — see GitSSHPortOrDefault.
	//
	// The port belongs to the instance, not to a repository: one bundle
	// owns one domain, and every repository on it answers at the same
	// endpoint (spec.md "Reaching the forge"). It is bundle identity, so a
	// restored or promoted instance answers on the same port with the same
	// host key and existing remotes keep working (RSTR-004).
	GitSSHPort int `yaml:"gitSshPort,omitempty"`

	Images            map[string]string  `yaml:"images"`
	Drivers           DriverConfig       `yaml:"drivers"`
	ACME              ACMEConfig         `yaml:"acme,omitempty"`
	Actions           ActionsConfig      `yaml:"actions,omitempty"`
	State             []StateDeclaration `yaml:"state"`
	ChecksumAlgorithm string             `yaml:"checksumAlgorithm"`
}

// Named reports whether the bundle owns a DNS name. False is a nameless
// bundle (INIT-005): no zone was proven, no certificate exists, and the
// instance's identity is whatever address it is served at. Callers that
// need a domain — TLS, the HTTPS root URL, a DNS record to flip — branch on
// this rather than comparing Domain to the empty string in a dozen places.
func (m *Manifest) Named() bool {
	return strings.TrimSpace(m.Domain) != ""
}

// GitSSHPortOrDefault reports the host port this bundle's git-over-SSH
// endpoint answers on: the manifest's own GitSSHPort, or DefaultGitSSHPort
// when it declares none. A manifest written before the field existed, or one
// an operator never touched, still resolves to the port UP-005 requires by
// default — the default is spelled in exactly one place, and every caller
// that publishes the port (deploy) or advertises it (forge's app.ini) reads
// it through here so the two can never disagree.
func (m *Manifest) GitSSHPortOrDefault() int {
	if m.GitSSHPort == 0 {
		return DefaultGitSSHPort
	}
	return m.GitSSHPort
}

// GitSSHCloneURL is the git-over-SSH clone URL for owner/repo on this
// bundle's instance, spelled the way Forgejo displays it (spec.md
// "Reaching the forge"): port 22 renders scp-style
// `git@domain:owner/repo.git`, and any other port carries the port in an
// `ssh://` URL.
//
// It lives here, on the manifest, because the URL is bundle identity —
// domain plus GitSSHPortOrDefault, nothing host-specific — and because
// more than one caller needs the same spelling: `up` reports it as the
// clone URL the operator can use, and `publish` writes it into a project's
// `origin` (IMPT-004). One function means the URL Farrier prints and the
// URL Farrier configures cannot drift apart.
func (m *Manifest) GitSSHCloneURL(owner, repo string) string {
	return m.GitSSHCloneURLAt(m.Domain, owner, repo)
}

// GitSSHCloneURLAt is GitSSHCloneURL for an instance reached at host rather
// than at the bundle domain — what a nameless bundle needs (UP-006), where
// the bundle carries no name and the operator supplies the address at `up`.
// The port and the spelling rules are the manifest's either way, so a
// nameless instance's clone URL is built by the same code as a named one's
// and cannot drift from it.
//
// host is spelled for a URL authority, so an IPv6 literal arrives already
// bracketed. A bracketed host always takes the `ssh://` form: scp-style
// `git@[::1]:owner/repo.git` is not a spelling git parses, and the colons
// inside the literal are exactly what makes the scp-style form ambiguous.
func (m *Manifest) GitSSHCloneURLAt(host, owner, repo string) string {
	port := m.GitSSHPortOrDefault()
	if port == 22 && !strings.HasPrefix(host, "[") {
		return fmt.Sprintf("git@%s:%s/%s.git", host, owner, repo)
	}
	return fmt.Sprintf("ssh://git@%s:%d/%s/%s.git", host, port, owner, repo)
}

// GitSSHKnownHostsHost is how this bundle's git-over-SSH endpoint is
// spelled on the left-hand side of an OpenSSH known_hosts line: a bare
// hostname on port 22, and `[domain]:port` on any other port, matching how
// OpenSSH itself keys non-default ports.
func (m *Manifest) GitSSHKnownHostsHost() string {
	port := m.GitSSHPortOrDefault()
	if port == 22 {
		return m.Domain
	}
	return fmt.Sprintf("[%s]:%d", m.Domain, port)
}

// ValidateGitSSHPort checks that port is one a manifest may carry: zero,
// meaning unset (GitSSHPortOrDefault), or a real TCP port. Exported so a
// frontend collecting the operator's choice can reject an impossible one
// up front — `init` checks it before spending an ACME exchange — rather
// than only when the assembled manifest is validated on its way to disk.
func ValidateGitSSHPort(port int) error {
	if port < 0 || port > maxPort {
		return fmt.Errorf("bundle: git-over-ssh port %d is not a valid TCP port", port)
	}
	return nil
}

// ColocatedRunnerEnabled reports whether this bundle wants the colocated
// Actions runner deployed (FORGE-005). An unset ColocatedRunner is enabled:
// a manifest written before the field existed, or one an operator never
// touched, still gets the runner FORGE-005 requires.
func (m *Manifest) ColocatedRunnerEnabled() bool {
	return m.Actions.ColocatedRunner == nil || *m.Actions.ColocatedRunner
}

// ColocatedRunnerDeclared reports whether the manifest states a colocated
// runner preference at all, as opposed to leaving it at its default. It
// separates "the operator asked for a colocated runner" from "nobody said",
// which is the difference between failing a deployment whose manifest pins
// no runner image and quietly skipping the runner on a bundle that predates
// the field (deploy.configureRunner).
func (m *Manifest) ColocatedRunnerDeclared() bool {
	return m.Actions.ColocatedRunner != nil
}

// NewManifest builds a manifest with all four state kinds declared and the
// default checksum algorithm set. An empty domain with a zero acmeCfg builds
// a nameless bundle (INIT-005); the two go together, and Validate rejects
// one without the other.
//
// It leaves Actions.ColocatedRunner and GitSSHPort unset rather than
// writing the defaults in: what a bundle deploys is init's policy, not this
// constructor's, and initialize.Run writes the operator's choices out
// explicitly so farrier.yaml shows both knobs. Unset still resolves to the
// same behavior (ColocatedRunnerEnabled, GitSSHPortOrDefault) — each
// default only ever has to be spelled in one place.
func NewManifest(domain string, images map[string]string, drivers DriverConfig, acmeCfg ACMEConfig) *Manifest {
	state := make([]StateDeclaration, len(AllStateKinds))
	for i, kind := range AllStateKinds {
		state[i] = StateDeclaration{Kind: kind}
	}
	return &Manifest{
		Domain:            domain,
		Images:            images,
		Drivers:           drivers,
		ACME:              acmeCfg,
		State:             state,
		ChecksumAlgorithm: DefaultChecksumAlgorithm,
	}
}

var digestPinned = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// Validate checks that a manifest is complete and internally consistent. It
// does not, and cannot, check that no secret ended up in Config — that's a
// property of the driver code that populates DriverRef, not of the manifest
// shape.
//
// The domain is optional, because a nameless bundle is a complete bundle
// (INIT-005). What is not optional is that the domain and the ACME section
// agree: a named bundle must say which DNS-01 provider proved its zone, and
// a nameless one must carry no ACME configuration at all. That pairing is
// what keeps "no domain" from being indistinguishable from a named manifest
// that lost its domain to a bad edit — the ACME section it kept is the tell,
// and it fails here rather than at the point something tries to renew a
// certificate for the empty string.
func (m *Manifest) Validate() error {
	if err := m.validateName(); err != nil {
		return err
	}
	if len(m.Images) == 0 {
		return fmt.Errorf("bundle: at least one image is required")
	}
	for component, ref := range m.Images {
		if !digestPinned.MatchString(ref) {
			return fmt.Errorf("bundle: image %q must be pinned by digest (@sha256:...), got %q", component, ref)
		}
	}
	if err := ValidateGitSSHPort(m.GitSSHPort); err != nil {
		return err
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

// validateName checks the domain against the ACME section. Named bundles
// need a DNS-01 provider — `up` and renewal reissue through it, so a named
// manifest without one is a bundle whose certificate expires with no way to
// replace it. Nameless bundles need the section empty, since nothing about
// them ever reaches ACME.
func (m *Manifest) validateName() error {
	if !m.Named() {
		if !m.ACME.isZero() {
			return fmt.Errorf("bundle: acme config is set but there is no domain; a nameless bundle proves no zone and issues no certificate")
		}
		return nil
	}
	if strings.TrimSpace(m.ACME.DNSProvider) == "" {
		return fmt.Errorf("bundle: acme dns-01 provider is required for domain %q", m.Domain)
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
