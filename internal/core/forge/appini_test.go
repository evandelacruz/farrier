package forge

import (
	"fmt"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"gopkg.in/yaml.v3"
)

const fakeDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func validManifest() *bundle.Manifest {
	return bundle.NewManifest(
		"forge.example.com",
		map[string]string{
			"forgejo": "codeberg.org/forgejo/forgejo@sha256:" + fakeDigest,
		},
		bundle.DriverConfig{
			Keystore: bundle.DriverRef{Driver: "file", Config: map[string]any{"path": "/keys/bundle.key"}},
			Blob:     bundle.DriverRef{Driver: "local", Config: map[string]any{"path": "/data/blobs"}},
		},
		bundle.ACMEConfig{DNSProvider: "manual"},
	)
}

func validSecrets() Secrets {
	return Secrets{
		SecretKey:     "secret-key-value",
		InternalToken: "internal-token-value",
		LFSJWTSecret:  "lfs-jwt-secret-value",
	}
}

func TestRenderAppINISkipsInstallWizard(t *testing.T) {
	out, err := RenderAppINI(validManifest(), validSecrets(), AppINIOptions{})
	if err != nil {
		t.Fatalf("RenderAppINI() error = %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "INSTALL_LOCK = true") {
		t.Fatalf("rendered app.ini does not lock the install wizard:\n%s", text)
	}
}

func TestRenderAppINIAnswersEveryWizardField(t *testing.T) {
	out, err := RenderAppINI(validManifest(), validSecrets(), AppINIOptions{})
	if err != nil {
		t.Fatalf("RenderAppINI() error = %v", err)
	}
	text := string(out)

	want := []string{
		"PROTOCOL = http\n",
		"DOMAIN = forge.example.com",
		"ROOT_URL = https://forge.example.com/",
		"SSH_DOMAIN = forge.example.com",
		"SSH_SERVER_HOST_KEYS = " + SSHHostKeyPath,
		"DB_TYPE = sqlite3",
		"PATH = /data/gitea/gitea.db",
		"ROOT = /data/git/repositories",
		"SECRET_KEY = secret-key-value",
		"INTERNAL_TOKEN = internal-token-value",
		"JWT_SECRET = lfs-jwt-secret-value",
		"ENABLED = true",
	}
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("rendered app.ini missing %q:\n%s", w, text)
		}
	}
}

// TestRenderAppINIServesPlaintextBehindCaddy guards against PROTOCOL = https:
// Caddy terminates TLS and proxies to Forgejo over plaintext, so Forgejo
// binding its own TLS server (no cert available) breaks the deployment.
func TestRenderAppINIServesPlaintextBehindCaddy(t *testing.T) {
	out, err := RenderAppINI(validManifest(), validSecrets(), AppINIOptions{})
	if err != nil {
		t.Fatalf("RenderAppINI() error = %v", err)
	}
	text := string(out)
	if strings.Contains(text, "PROTOCOL = https") {
		t.Fatalf("rendered app.ini binds Forgejo's own TLS server:\n%s", text)
	}
	if !strings.Contains(text, "PROTOCOL = http\n") {
		t.Fatalf("rendered app.ini does not serve plaintext behind Caddy:\n%s", text)
	}
}

// TestRenderAppINIEnablesActionsForForkPRApproval guards FORGE-003. Forgejo's
// fork-PR approval gate is unconditional once Actions is enabled — there is no
// app.ini key or per-repository setting that loosens it — so this one section
// is the entire mechanism behind the CI trust boundary in spec.md. Rendering
// [actions] exactly once also keeps the file valid: a duplicate section header
// would silently shadow the first.
func TestRenderAppINIEnablesActionsForForkPRApproval(t *testing.T) {
	out, err := RenderAppINI(validManifest(), validSecrets(), AppINIOptions{})
	if err != nil {
		t.Fatalf("RenderAppINI() error = %v", err)
	}
	text := string(out)

	if !strings.Contains(text, "[actions]\nENABLED = true\n") {
		t.Errorf("rendered app.ini does not enable Actions, so the fork-PR approval gate is off:\n%s", text)
	}
	if got := strings.Count(text, "[actions]"); got != 1 {
		t.Errorf("rendered app.ini has %d [actions] sections, want exactly 1:\n%s", got, text)
	}
}

func TestRenderAppINIRequiresValidManifest(t *testing.T) {
	m := validManifest()
	m.Domain = ""
	if _, err := RenderAppINI(m, validSecrets(), AppINIOptions{}); err == nil {
		t.Fatal("RenderAppINI() with invalid manifest = nil error, want error")
	}
}

func TestRenderAppININilManifest(t *testing.T) {
	if _, err := RenderAppINI(nil, validSecrets(), AppINIOptions{}); err == nil {
		t.Fatal("RenderAppINI(nil, ...) = nil error, want error")
	}
}

func TestRenderAppINIRequiresSecrets(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Secrets)
	}{
		{"missing secret key", func(s *Secrets) { s.SecretKey = "" }},
		{"missing internal token", func(s *Secrets) { s.InternalToken = "" }},
		{"missing lfs jwt secret", func(s *Secrets) { s.LFSJWTSecret = "" }},
		{"secret key with newline", func(s *Secrets) { s.SecretKey = "bad\nvalue" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSecrets()
			tc.mutate(&s)
			if _, err := RenderAppINI(validManifest(), s, AppINIOptions{}); err == nil {
				t.Fatalf("RenderAppINI() with %s = nil error, want error", tc.name)
			}
		})
	}
}

// TestRenderAppININeverEmbedsSecretsInTheBundle guards KEY-003: the rendered
// app.ini carries secrets by design (Forgejo needs them to run), but nothing
// about that rendering should route through the manifest, which is bundle
// content and must stay clean of key material.
func TestRenderAppININeverEmbedsSecretsInTheBundle(t *testing.T) {
	m := validManifest()
	if _, err := RenderAppINI(m, validSecrets(), AppINIOptions{}); err != nil {
		t.Fatalf("RenderAppINI() error = %v", err)
	}
	raw, err := yaml.Marshal(m)
	if err != nil {
		t.Fatalf("yaml.Marshal(manifest) error = %v", err)
	}
	for _, secret := range []string{"secret-key-value", "internal-token-value", "lfs-jwt-secret-value"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("manifest YAML contains secret material %q", secret)
		}
	}
}

// TestRenderAppINIQuarantineDisablesEveryOutboundPath is DRIL-002's
// config half. Each key closes a different door, so each is asserted
// separately: turning webhooks off does nothing about email, and turning
// both off does nothing about a push mirror writing to production's real
// remote.
func TestRenderAppINIQuarantineDisablesEveryOutboundPath(t *testing.T) {
	out, err := RenderAppINI(validManifest(), validSecrets(), AppINIOptions{Quarantine: true})
	if err != nil {
		t.Fatalf("RenderAppINI() error = %v", err)
	}
	text := string(out)

	for _, tc := range []struct {
		door string
		want string
	}{
		{"repository and system webhooks", "DISABLE_WEBHOOKS = true"},
		{"webhook delivery hosts", "[webhook]\nALLOWED_HOST_LIST =\n"},
		{"outbound email", "[mailer]\nENABLED = false\n"},
		{"push mirrors", "[mirror]\nENABLED = false\n"},
	} {
		if !strings.Contains(text, tc.want) {
			t.Errorf("quarantined app.ini leaves %s open (want %q):\n%s", tc.door, tc.want, text)
		}
	}

	// Duplicate section headers silently shadow the first, so each
	// quarantine section must be rendered exactly once — the same hazard
	// TestRenderAppINIEnablesActionsForForkPRApproval guards for [actions].
	for _, section := range []string{"[security]", "[webhook]", "[mailer]", "[mirror]"} {
		if got := strings.Count(text, section); got != 1 {
			t.Errorf("quarantined app.ini has %d %s sections, want exactly 1:\n%s", got, section, text)
		}
	}
}

// TestRenderAppINIQuarantineKeepsActionsEnabled pins the one thing
// quarantine must not shut off: a drill exists to prove CI runs on the
// restored instance (DRIL-001), which it cannot do with Actions disabled.
func TestRenderAppINIQuarantineKeepsActionsEnabled(t *testing.T) {
	out, err := RenderAppINI(validManifest(), validSecrets(), AppINIOptions{Quarantine: true})
	if err != nil {
		t.Fatalf("RenderAppINI() error = %v", err)
	}
	if !strings.Contains(string(out), "[actions]\nENABLED = true\n") {
		t.Errorf("quarantined app.ini disables Actions, so a drill cannot run its smoke job:\n%s", out)
	}
}

// TestRenderAppINIWithoutQuarantineIsUnchanged guards the boundary the
// other direction: quarantine is a drill-only override, and a production
// deployment that quietly stopped delivering webhooks or email would be a
// silent outage of exactly the kind nobody notices for a week.
func TestRenderAppINIWithoutQuarantineIsUnchanged(t *testing.T) {
	out, err := RenderAppINI(validManifest(), validSecrets(), AppINIOptions{})
	if err != nil {
		t.Fatalf("RenderAppINI() error = %v", err)
	}
	text := string(out)

	for _, unwanted := range []string{"DISABLE_WEBHOOKS", "[webhook]", "[mailer]", "[mirror]"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("ordinary app.ini carries quarantine override %q:\n%s", unwanted, text)
		}
	}
}

// TestRenderAppINIQuarantineKeepsIdentityIntact pins that quarantine
// changes what the instance may do, never who it is. A drill that rendered
// a different SECRET_KEY, domain, or SSH host key would no longer be
// rehearsing the snapshot it restored (spec.md "Rehearsal").
func TestRenderAppINIQuarantineKeepsIdentityIntact(t *testing.T) {
	m, secrets := validManifest(), validSecrets()

	quarantined, err := RenderAppINI(m, secrets, AppINIOptions{Quarantine: true})
	if err != nil {
		t.Fatalf("RenderAppINI() error = %v", err)
	}
	text := string(quarantined)

	for _, want := range []string{
		"DOMAIN = " + m.Domain,
		"ROOT_URL = https://" + m.Domain + "/",
		"SECRET_KEY = " + secrets.SecretKey,
		"INTERNAL_TOKEN = " + secrets.InternalToken,
		"JWT_SECRET = " + secrets.LFSJWTSecret,
		"SSH_SERVER_HOST_KEYS = " + SSHHostKeyPath,
		"INSTALL_LOCK = true",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("quarantined app.ini changed identity, missing %q:\n%s", want, text)
		}
	}
}

// namelessManifest is what `init` with no domain produces (INIT-005): no
// domain, and no ACME section to pair with one.
func namelessManifest() *bundle.Manifest {
	m := validManifest()
	m.Domain = ""
	m.ACME = bundle.ACMEConfig{}
	return m
}

// INIT-005 + UP-006: every URL in app.ini is built from one host, and a
// nameless bundle carries none — the operator supplies it at `up`. Without
// it this fails loudly rather than deploying a Forgejo whose ROOT_URL is
// "https:///".
func TestRenderAppINIRejectsANamelessBundleWithoutAnAddress(t *testing.T) {
	_, err := RenderAppINI(namelessManifest(), validSecrets(), AppINIOptions{})
	if err == nil {
		t.Fatal("RenderAppINI: want error for a nameless bundle with no address, got nil")
	}
	if !strings.Contains(err.Error(), "UP-006") {
		t.Errorf("error = %v, want it to point at UP-006", err)
	}
}

// UP-006: with the operator's address, every URL a nameless instance
// advertises is built from it, over plain HTTP.
func TestRenderAppINIRendersANamelessBundleAtTheSuppliedAddress(t *testing.T) {
	out, err := RenderAppINI(namelessManifest(), validSecrets(), AppINIOptions{Address: "192.168.1.5"})
	if err != nil {
		t.Fatalf("RenderAppINI: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"ROOT_URL = http://192.168.1.5/",
		"DOMAIN = 192.168.1.5",
		"SSH_DOMAIN = 192.168.1.5",
		// Unchanged from the named case: Caddy is what terminates, and it
		// proxies to Forgejo in plaintext either way.
		"PROTOCOL = http",
		// UP-006's "git over SSH unchanged".
		"START_SSH_SERVER = true",
		"SSH_SERVER_HOST_KEYS = " + SSHHostKeyPath,
		"INSTALL_LOCK = true",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("nameless app.ini missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "https://") {
		t.Errorf("nameless app.ini advertises an https URL:\n%s", text)
	}
}

// A named bundle already answers where it is reached, so an address on top
// of it is a conflict rather than an override.
func TestRenderAppINIRejectsAnAddressForANamedBundle(t *testing.T) {
	_, err := RenderAppINI(validManifest(), validSecrets(), AppINIOptions{Address: "192.168.1.5"})
	if err == nil {
		t.Fatal("RenderAppINI: want error for an address on a named bundle, got nil")
	}
}

// The admin account's email has to be a valid one, and a nameless instance
// has no name to build it from — an IP literal would not be a valid email
// domain at all (UP-006).
func TestAdminEmailDomainFallsBackToAReservedDomainWhenNameless(t *testing.T) {
	if got, want := AdminEmailDomain(validManifest()), "forge.example.com"; got != want {
		t.Errorf("AdminEmailDomain(named) = %q, want %q", got, want)
	}
	got := AdminEmailDomain(namelessManifest())
	if !strings.HasSuffix(got, ".invalid") {
		t.Errorf("AdminEmailDomain(nameless) = %q, want a reserved .invalid domain", got)
	}
	account, err := NewAdminAccount(got)
	if err != nil {
		t.Fatalf("NewAdminAccount(%q): %v", got, err)
	}
	if !strings.Contains(account.Email, "@") {
		t.Errorf("admin email = %q, want a well-formed address", account.Email)
	}
}

// TestRenderAppINIAdvertisesTheManifestSSHPort is UP-005's "the SSH clone
// URL Forgejo displays is the one that works": SSH_PORT, which is what
// Forgejo renders into displayed clone URLs, is the *host* port the
// manifest declares, while SSH_LISTEN_PORT is the container-side port the
// builtin server actually binds — the port `up` publishes that host port
// onto.
func TestRenderAppINIAdvertisesTheManifestSSHPort(t *testing.T) {
	cases := []struct {
		name     string
		declared int
		want     int
	}{
		{"unset takes the bundle default", 0, bundle.DefaultGitSSHPort},
		{"explicit 22", 22, 22},
		{"explicit high port", 2022, 2022},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest()
			m.GitSSHPort = tc.declared

			out, err := RenderAppINI(m, validSecrets(), AppINIOptions{})
			if err != nil {
				t.Fatalf("RenderAppINI() error = %v", err)
			}
			text := string(out)

			for _, want := range []string{
				fmt.Sprintf("SSH_PORT = %d\n", tc.want),
				fmt.Sprintf("SSH_LISTEN_PORT = %d\n", SSHListenPort),
				"START_SSH_SERVER = true",
				"SSH_DOMAIN = forge.example.com",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("rendered app.ini missing %q:\n%s", want, text)
				}
			}
		})
	}
}

// TestSSHListenPortIsUnprivileged guards the reason SSHListenPort is not 22:
// Forgejo's builtin SSH server runs as RUN_USER inside the container, and a
// non-root process cannot bind a privileged port — so a container-side port
// below 1024 would fail to listen whatever host port were published onto it.
func TestSSHListenPortIsUnprivileged(t *testing.T) {
	if SSHListenPort <= 1024 {
		t.Errorf("SSHListenPort = %d, want an unprivileged port the container's non-root RUN_USER can bind", SSHListenPort)
	}
}

// TestRenderAppINIQuarantineKeepsTheSSHEndpoint pins that drill mode does
// not move the git-over-SSH endpoint: a drill rehearses the snapshot's own
// identity, and where it is reachable *from* is the deploy step's job
// (deploy.publishGitSSH binds it to loopback), not this renderer's.
func TestRenderAppINIQuarantineKeepsTheSSHEndpoint(t *testing.T) {
	m := validManifest()
	m.GitSSHPort = 2022

	ordinary, err := RenderAppINI(m, validSecrets(), AppINIOptions{})
	if err != nil {
		t.Fatalf("RenderAppINI() error = %v", err)
	}
	quarantined, err := RenderAppINI(m, validSecrets(), AppINIOptions{Quarantine: true})
	if err != nil {
		t.Fatalf("RenderAppINI(quarantine) error = %v", err)
	}

	for _, want := range []string{"SSH_PORT = 2022\n", "SSH_DOMAIN = forge.example.com", "SSH_SERVER_HOST_KEYS = " + SSHHostKeyPath} {
		if !strings.Contains(string(ordinary), want) || !strings.Contains(string(quarantined), want) {
			t.Errorf("quarantine changed the git-over-ssh identity: %q not in both renders", want)
		}
	}
}
