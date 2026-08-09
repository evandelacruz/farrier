package forge

import (
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
	out, err := RenderAppINI(validManifest(), validSecrets())
	if err != nil {
		t.Fatalf("RenderAppINI() error = %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "INSTALL_LOCK = true") {
		t.Fatalf("rendered app.ini does not lock the install wizard:\n%s", text)
	}
}

func TestRenderAppINIAnswersEveryWizardField(t *testing.T) {
	out, err := RenderAppINI(validManifest(), validSecrets())
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
	out, err := RenderAppINI(validManifest(), validSecrets())
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
	out, err := RenderAppINI(validManifest(), validSecrets())
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
	if _, err := RenderAppINI(m, validSecrets()); err == nil {
		t.Fatal("RenderAppINI() with invalid manifest = nil error, want error")
	}
}

func TestRenderAppININilManifest(t *testing.T) {
	if _, err := RenderAppINI(nil, validSecrets()); err == nil {
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
			if _, err := RenderAppINI(validManifest(), s); err == nil {
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
	if _, err := RenderAppINI(m, validSecrets()); err != nil {
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
