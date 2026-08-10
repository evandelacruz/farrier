package orchestrate

import (
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"gopkg.in/yaml.v3"
)

func testManifest() *bundle.Manifest {
	return bundle.NewManifest("example.com", map[string]string{
		"forgejo": "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("a", 64),
		"caddy":   "docker.io/library/caddy@sha256:" + strings.Repeat("b", 64),
	}, bundle.DriverConfig{
		Keystore: bundle.DriverRef{Driver: "file"},
		Blob:     bundle.DriverRef{Driver: "local"},
	}, bundle.ACMEConfig{DNSProvider: "manual"})
}

func TestRenderProducesOneServicePerImage(t *testing.T) {
	files, err := Render(testManifest())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	raw, ok := files[ComposeFile]
	if !ok {
		t.Fatalf("Render: missing %s, got %v", ComposeFile, keys(files))
	}

	var spec composeSpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("unmarshal rendered compose: %v", err)
	}

	if len(spec.Services) != 2 {
		t.Fatalf("services = %d, want 2: %+v", len(spec.Services), spec.Services)
	}
	forgejo, ok := spec.Services["forgejo"]
	if !ok {
		t.Fatalf("missing forgejo service: %+v", spec.Services)
	}
	if forgejo.Image != "codeberg.org/forgejo/forgejo@sha256:"+strings.Repeat("a", 64) {
		t.Errorf("forgejo image = %q", forgejo.Image)
	}
	if forgejo.ContainerName != "farrier-forgejo" {
		t.Errorf("forgejo container_name = %q", forgejo.ContainerName)
	}
	if forgejo.Restart != "unless-stopped" {
		t.Errorf("forgejo restart = %q", forgejo.Restart)
	}
	if len(forgejo.Networks) != 1 || forgejo.Networks[0] != networkName {
		t.Errorf("forgejo networks = %v", forgejo.Networks)
	}
	if forgejo.Environment["FARRIER_DOMAIN"] != "example.com" {
		t.Errorf("forgejo FARRIER_DOMAIN = %q", forgejo.Environment["FARRIER_DOMAIN"])
	}

	if net, ok := spec.Networks[networkName]; !ok || net.Name != networkName {
		t.Errorf("networks = %+v", spec.Networks)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	m := testManifest()
	first, err := Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	second, err := Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if string(first[ComposeFile]) != string(second[ComposeFile]) {
		t.Errorf("Render output is not deterministic:\n---\n%s\n---\n%s", first[ComposeFile], second[ComposeFile])
	}
}

func TestRenderRejectsInvalidManifest(t *testing.T) {
	m := testManifest()
	m.Domain = ""
	if _, err := Render(m); err == nil {
		t.Fatal("Render: want error for invalid manifest, got nil")
	}
}

func TestRenderRejectsNilManifest(t *testing.T) {
	if _, err := Render(nil); err == nil {
		t.Fatal("Render: want error for nil manifest, got nil")
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// INIT-005: a nameless bundle has no domain, so its services carry no
// FARRIER_DOMAIN — an empty value would read as a name that failed to
// render.
func TestRenderOmitsTheDomainForANamelessBundle(t *testing.T) {
	m := testManifest()
	m.Domain = ""
	m.ACME = bundle.ACMEConfig{}

	files, err := Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var spec composeSpec
	if err := yaml.Unmarshal(files[ComposeFile], &spec); err != nil {
		t.Fatalf("unmarshal rendered compose: %v", err)
	}
	for name, service := range spec.Services {
		if _, ok := service.Environment["FARRIER_DOMAIN"]; ok {
			t.Errorf("service %q carries FARRIER_DOMAIN = %q, want it absent", name, service.Environment["FARRIER_DOMAIN"])
		}
	}
}
