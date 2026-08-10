// Package bundle defines the bundle directory format: the manifest and
// rendered Compose files that together describe a Farrier deployment.
//
// The manifest never carries a secret. Driver config holds only references
// to where secrets live (a keystore driver name plus its non-secret config,
// e.g. a file path or a command line) — never a secret value itself. That,
// plus the fact that Bundle is loaded and saved purely from a directory
// path with no host-specific state retained, is what makes a bundle
// "function identically after being copied to another machine, given key
// access" (CORE-001).
//
// The one piece of key material the manifest does carry is the SSH host
// key's public half (SSHHostKeyPublic), which is public by definition —
// the same string an operator would paste into known_hosts. CORE-001 draws
// the line at secrecy, not at the phrase "key material": a bundle is meant
// to be copied, and a fingerprint a reader can verify the instance against
// is exactly the kind of thing that should travel with it.
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

// DefaultNamedWebPort and DefaultNamelessWebPort are the host ports `up`
// publishes Caddy on when a manifest declares none — the web endpoint's
// counterpart to DefaultGitSSHPort, resolved by WebPortOrDefault.
//
// A named bundle takes 443 because that is where an HTTPS client looks
// without being told, and a named instance is meant to be handed to a team
// as a bare domain.
//
// A nameless one takes 8222 rather than 80. Port 80 is the most contended
// port on a developer's machine, and the nameless tier (INIT-005) exists
// precisely so Farrier can be tried on the machine the operator is sitting
// at — a default that collides with whatever else is already listening
// turns the first command a new operator runs into a Docker
// "port is already allocated" failure. 8222 keeps the web port visibly
// related to DefaultGitSSHPort while staying clear of 3000, 5000, 8000,
// and 8080, which the things already running on that machine tend to own.
//
// Neither default is an assumption Farrier is entitled to: the operator
// brings the host and may already be serving something on either port, so
// both are manifest fields (Manifest.WebPort) and both are theirs to move.
const (
	DefaultNamedWebPort    = 443
	DefaultNamelessWebPort = 8222
)

// standardHTTPSPort and standardHTTPPort are the ports a URL of each scheme
// implies, and therefore the ports WebURL leaves out of the URL it renders.
const (
	standardHTTPSPort = 443
	standardHTTPPort  = 80
)

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

// Manifest is the bundle's farrier.yaml: domain, published web port and the
// public one, git-over-SSH host port, pinned image digests, driver config,
// ACME DNS-01 config, CI runner config, state-kind declarations, and the
// checksum algorithm used throughout backup and restore.
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

	// WebPort is the host port `up` publishes the instance's web endpoint
	// on — the host side of Caddy's port mapping, and nothing else. Zero
	// means unset and resolves through WebPortOrDefault to
	// DefaultNamedWebPort or DefaultNamelessWebPort depending on whether
	// the bundle carries a name.
	//
	// Only the host side is a choice. Caddy binds inside its own network
	// namespace, where nothing else of the operator's is listening, so the
	// container port never moves and never collides.
	//
	// It is deliberately separate from PublicWebPort. This field says where
	// Caddy listens; that one says where clients connect. The two are the
	// same number whenever Farrier's Caddy is the edge, which is the normal
	// case and the reason PublicWebPort is usually empty.
	WebPort int `yaml:"webPort,omitempty"`

	// PublicWebPort is the port clients actually reach this instance on,
	// when that is not the port Caddy is published at. Zero means unset and
	// resolves through PublicWebPortOrDefault to WebPortOrDefault — Caddy
	// is the edge and the two are one number.
	//
	// It exists for exactly one configuration: something already on the
	// host holds the standard port and forwards to Farrier. Then Caddy is
	// published somewhere else, while every URL the forge renders must
	// still name the port clients connect to, or the instance advertises an
	// endpoint nothing answers on.
	//
	// A fronting proxy has to pass TCP through — SNI routing — and let
	// Caddy terminate TLS. Farrier owns the certificate: identity lives in
	// the bundle, and restore and promote promise an unchanged TLS identity
	// on new hardware. A proxy that terminates TLS itself hands clients a
	// certificate that is not the bundle's and breaks that promise (spec.md
	// "Reaching the forge").
	//
	// Farrier cannot see the proxy, so this field is the operator asserting
	// it. That is why a named bundle published somewhere other than
	// DefaultNamedWebPort must set it — see ValidateWebPorts.
	PublicWebPort int `yaml:"publicWebPort,omitempty"`

	// SSHHostKeyPublic is the public half of the instance's SSH host key,
	// in OpenSSH authorized-keys format — the fingerprint a client checks
	// the git-over-SSH endpoint against. `init` writes it here from the
	// keystore, which stays the source of truth and keeps the private half
	// (INIT-003); this is a copy for readers who should not hold the
	// keystore.
	//
	// It is here rather than only in the keystore because pinning a host
	// key is not a privileged act. `publish` renders it into a known_hosts
	// entry so a host answering with a different key fails the push
	// (IMPT-004), and reading it out of the keystore meant anyone who
	// publishes to an instance needs read access to the store holding
	// SECRET_KEY, INTERNAL_TOKEN, and the age backup key. That is what
	// blocked the shared instance the design supports (spec.md "The unit:
	// one forge per project"): one forge, several projects, each project's
	// owner publishing to it.
	//
	// Empty is a manifest written before the field existed. Readers fall
	// back to the keystore rather than skipping the pin — see
	// publish.knownHostsLine.
	SSHHostKeyPublic string `yaml:"sshHostKeyPublic,omitempty"`

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

// WebPortOrDefault reports the host port `up` publishes this bundle's web
// endpoint on: the manifest's own WebPort, or the default for the tier the
// bundle is in when it declares none. Named and nameless have different
// defaults (DefaultNamedWebPort, DefaultNamelessWebPort), so the answer
// changes when a nameless bundle is given a name — see attach.
//
// Every caller that publishes the port or advertises it reads it through
// here, the same way GitSSHPortOrDefault is the one answer for git over
// SSH, so the port Compose binds and the port a URL names cannot disagree.
func (m *Manifest) WebPortOrDefault() int {
	if m.WebPort != 0 {
		return m.WebPort
	}
	if m.Named() {
		return DefaultNamedWebPort
	}
	return DefaultNamelessWebPort
}

// PublicWebPortOrDefault reports the port clients reach this instance on:
// the manifest's own PublicWebPort, or the published port when it declares
// none. Unset is the ordinary case — Caddy is the edge, so where it listens
// and where clients connect are one number.
func (m *Manifest) PublicWebPortOrDefault() int {
	if m.PublicWebPort != 0 {
		return m.PublicWebPort
	}
	return m.WebPortOrDefault()
}

// WebScheme is how browsers reach this instance: HTTPS for a named bundle,
// which `up` completes with a certificate serving at its domain (UP-002),
// and plain HTTP for a nameless one (UP-006). Caddy terminates in both
// cases; only the outer hop's encryption differs.
func (m *Manifest) WebScheme() string {
	if m.Named() {
		return "https"
	}
	return "http"
}

// WebURL renders the instance's URL at host on port, with the port left out
// when it is the one the scheme already implies — 443 for https, 80 for
// http. Omitting it is not cosmetic: the URL lands in ROOT_URL, in every
// clone URL Forgejo displays, and in the runner's registration, and an
// operator handed `https://git.example.com:443/` would reasonably wonder
// what else about their instance is non-standard.
//
// host is spelled for a URL authority, so an IPv6 literal arrives already
// bracketed. The trailing slash is Forgejo's ROOT_URL convention and every
// caller here wants the same spelling.
func (m *Manifest) WebURL(host string, port int) string {
	host = strings.TrimSpace(host)
	scheme := m.WebScheme()
	if (scheme == "https" && port == standardHTTPSPort) || (scheme == "http" && port == standardHTTPPort) {
		return fmt.Sprintf("%s://%s/", scheme, host)
	}
	return fmt.Sprintf("%s://%s:%d/", scheme, host, port)
}

// PublicURLAt is the URL clients use to reach this instance at host: the
// bundle's scheme, the given host, and the public port. It is what
// ROOT_URL, the clone URLs Forgejo renders, and runner registration all
// have to say, because it is what something is actually listening on.
//
// host is the bundle domain for a named bundle and the operator-supplied
// address for a nameless one; forge.InstanceURL picks between them so no
// caller has to.
func (m *Manifest) PublicURLAt(host string) string {
	return m.WebURL(host, m.PublicWebPortOrDefault())
}

// PublicURL is PublicURLAt at this bundle's own domain — the URL a named
// instance is reached at. It is meaningless for a nameless bundle, which
// carries no host of its own; use PublicURLAt with the address instead.
func (m *Manifest) PublicURL() string {
	return m.PublicURLAt(m.Domain)
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
	return m.GitSSHKnownHostsHostAt(m.Domain)
}

// GitSSHKnownHostsHostAt is GitSSHKnownHostsHost for an instance reached at
// host rather than at the bundle domain — the nameless case (UP-006), where
// the address is the identity. It exists for the same reason
// GitSSHCloneURLAt does, and the two must agree: `publish` pins the push
// with a known_hosts line and points `origin` at a clone URL, and a line
// naming one host while the URL names another fails the push with an opaque
// host-key error (IMPT-004).
//
// host is spelled for a URL authority, so an IPv6 literal arrives
// bracketed. Those brackets are stripped first: a known_hosts entry's
// brackets belong to the port, so OpenSSH keys `fd00::1` on port 22 and
// `[fd00::1]:2222` on any other — never `[[fd00::1]]:2222`. The default git
// SSH port is 2222, so a nameless instance always takes the bracketed form.
func (m *Manifest) GitSSHKnownHostsHostAt(host string) string {
	host = unbracketHost(host)
	port := m.GitSSHPortOrDefault()
	if port == 22 {
		return host
	}
	return fmt.Sprintf("[%s]:%d", host, port)
}

// unbracketHost removes the brackets a URL authority puts around an IPv6
// literal, leaving every other host untouched.
func unbracketHost(host string) string {
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		return host[1 : len(host)-1]
	}
	return host
}

// SSHKnownHostsLineFor renders publicKey as an OpenSSH known_hosts entry
// pinning this bundle's git-over-SSH endpoint: the endpoint's known_hosts
// spelling, then the key's type and blob.
//
// The comment field of an authorized-keys line is dropped, because it is
// not part of a known_hosts entry — OpenSSH would read whatever follows the
// blob as a further host-key option.
//
// It takes the key rather than reading SSHHostKeyPublic so that a caller
// falling back to the keystore for a manifest written before that field
// existed gets a line rendered by the same code. One renderer means the two
// sources cannot produce entries that differ.
func (m *Manifest) SSHKnownHostsLineFor(publicKey string) (string, error) {
	return m.SSHKnownHostsLineAt(m.Domain, publicKey)
}

// SSHKnownHostsLineAt is SSHKnownHostsLineFor for an instance reached at
// host rather than at the bundle domain (UP-006). It is the one renderer
// both cases go through, so the entry a nameless instance is pinned with
// and the entry a named one is pinned with differ only in the host.
func (m *Manifest) SSHKnownHostsLineAt(host, publicKey string) (string, error) {
	keyType, blob, err := SplitSSHPublicKey(publicKey)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %s %s\n", m.GitSSHKnownHostsHostAt(host), keyType, blob), nil
}

// SplitSSHPublicKey pulls the type and base64 blob out of an OpenSSH
// authorized-keys line, discarding any comment. It reports the shape it
// wanted rather than the bytes it got: a host key is public, but a value
// that came out of a keystore is never echoed into an error, an event, or a
// log (KEY-003), and one function that is safe in both cases is better than
// two that differ only in what they may print.
func SplitSSHPublicKey(line string) (keyType, blob string, err error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		return "", "", fmt.Errorf("is not an openssh public key: want \"<type> <base64> [comment]\"")
	}
	return fields[0], fields[1], nil
}

// domainPattern is the grammar a bundle domain has to match: one or more
// dot-separated labels of letters, digits, and hyphens (no label starting
// or ending with one), ending in an alphabetic TLD of at least two
// characters. A fully-qualified name, in other words — the thing spec.md
// "The domain" means by a name the operator controls in DNS.
var domainPattern = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`)

// ValidateDomain checks that domain is a well-formed FQDN a bundle may
// carry. It lives here, beside the Domain field, because both operations
// that put a name on a bundle need the same answer: `init` when it builds a
// named bundle (INIT-002) and `attach` when it names a nameless one in
// place (UP-007). One grammar in one place is what keeps a name init would
// accept from being a name attach rejects.
//
// Manifest.Validate deliberately does not call it. A manifest already on
// disk was accepted by whichever operation wrote it, and tightening the
// grammar later must not turn a running instance's bundle into one that no
// longer loads. This is the front-door check on an operator's input, not a
// retroactive one on stored state.
func ValidateDomain(domain string) error {
	d := strings.TrimSpace(domain)
	if d == "" {
		return fmt.Errorf("bundle: domain is required")
	}
	if !domainPattern.MatchString(d) {
		return fmt.Errorf("bundle: domain %q is not a valid DNS name", domain)
	}
	return nil
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

// ValidateWebPort checks that port is one a manifest may carry as a web
// port: zero, meaning unset, or a real TCP port. Exported for the same
// reason ValidateGitSSHPort is — a frontend collecting the operator's
// choice rejects an impossible one up front.
func ValidateWebPort(port int) error {
	if port < 0 || port > maxPort {
		return fmt.Errorf("bundle: web port %d is not a valid TCP port", port)
	}
	return nil
}

// ValidateWebPorts checks that the published web port and the public one
// agree about what clients should be told, and refuses a named bundle that
// leaves the question open.
//
// Moving a named instance's published port off 443 has two possible
// meanings, and they produce different URLs:
//
//   - nothing else is on 443, so clients connect to the moved port and
//     every URL the forge renders must carry it
//   - something on 443 forwards to Farrier, so clients connect to 443 and
//     no URL should mention the moved port at all
//
// Farrier cannot tell which. It sees one Docker daemon on one host and has
// no way to know what is bound in front of it, and guessing wrong is worse
// than refusing: the forge comes up healthy while every clone URL, webhook
// target, and runner registration it hands out points at an endpoint
// nothing answers on. So the operator states it, by setting PublicWebPort
// — to 443 when a proxy fronts the instance, or to the published port when
// nothing does. Saying the same number twice is not redundant; it is the
// assertion that Caddy is the edge.
//
// A nameless bundle is exempt. It is served over plain HTTP at an address
// the operator supplies on a trusted network (UP-006), nothing fronts it,
// and its default published port is already non-standard — requiring a
// second field to confirm the first would be a question with one possible
// answer.
//
// It is exported so `up` and `attach` can refuse before they touch a host
// or spend an ACME exchange, and it is called from Validate so no path
// writes a manifest that cannot be deployed.
func (m *Manifest) ValidateWebPorts() error {
	if err := ValidateWebPort(m.WebPort); err != nil {
		return err
	}
	if err := ValidateWebPort(m.PublicWebPort); err != nil {
		return err
	}
	if !m.Named() || m.PublicWebPort != 0 {
		return nil
	}
	if published := m.WebPortOrDefault(); published != DefaultNamedWebPort {
		return fmt.Errorf(
			"bundle: this bundle publishes its web port on %d rather than %d, so set publicWebPort in %s to say which port clients connect to: %d if something on the host forwards to Farrier, or %d if nothing does and clients reach it directly",
			published, DefaultNamedWebPort, ManifestFile, DefaultNamedWebPort, published,
		)
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
	if err := m.ValidateWebPorts(); err != nil {
		return err
	}
	// Shape-checked, not merely carried. An unreadable entry here is a
	// host-key pin that would fail at the point a push is being made, or —
	// worse, if a reader were lenient — one that quietly stopped pinning
	// anything. Empty is fine: that is a manifest written before the field
	// existed, and readers fall back to the keystore.
	if strings.TrimSpace(m.SSHHostKeyPublic) != "" {
		if _, _, err := SplitSSHPublicKey(m.SSHHostKeyPublic); err != nil {
			return fmt.Errorf("bundle: ssh host public key %w", err)
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
