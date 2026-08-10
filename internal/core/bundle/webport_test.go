package bundle

import (
	"strings"
	"testing"
)

// The four configurations a bundle's web endpoint can be in, and the two
// numbers each produces: the host port `up` publishes Caddy on, and the URL
// clients are told to use. They are separate answers on purpose — see
// Manifest.WebPort — and everything else in this file is a consequence of
// this table.
func TestWebPortAndPublicURLAcrossEveryConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name          string
		domain        string
		address       string
		webPort       int
		publicWebPort int
		wantPublished int
		wantURL       string
	}{
		{
			name:          "nameless, default",
			address:       "box.tail1234.ts.net",
			wantPublished: DefaultNamelessWebPort,
			wantURL:       "http://box.tail1234.ts.net:8222/",
		},
		{
			name:          "named, default",
			domain:        "forge.example.com",
			wantPublished: DefaultNamedWebPort,
			wantURL:       "https://forge.example.com/",
		},
		{
			name:          "named, published moved, nothing fronting it",
			domain:        "forge.example.com",
			webPort:       8443,
			publicWebPort: 8443,
			wantPublished: 8443,
			wantURL:       "https://forge.example.com:8443/",
		},
		{
			name:          "named, published moved, a proxy on 443 forwards to it",
			domain:        "forge.example.com",
			webPort:       8443,
			publicWebPort: 443,
			wantPublished: 8443,
			wantURL:       "https://forge.example.com/",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manifest{Domain: tc.domain, WebPort: tc.webPort, PublicWebPort: tc.publicWebPort}
			if err := m.ValidateWebPorts(); err != nil {
				t.Fatalf("ValidateWebPorts: %v", err)
			}
			if got := m.WebPortOrDefault(); got != tc.wantPublished {
				t.Errorf("WebPortOrDefault() = %d, want %d", got, tc.wantPublished)
			}
			host := tc.domain
			if host == "" {
				host = tc.address
			}
			if got := m.PublicURLAt(host); got != tc.wantURL {
				t.Errorf("PublicURLAt(%q) = %q, want %q", host, got, tc.wantURL)
			}
		})
	}
}

// The port is in the URL unless the scheme already implies it. 443 on https
// and 80 on http are the two the URL leaves out; every other port, in
// either scheme, is carried — a URL that omitted it would name an endpoint
// nothing answers on.
func TestWebURLOmitsOnlyTheSchemesOwnPort(t *testing.T) {
	named := &Manifest{Domain: "forge.example.com"}
	nameless := &Manifest{}

	for _, tc := range []struct {
		m    *Manifest
		port int
		want string
	}{
		{named, 443, "https://forge.example.com/"},
		{named, 8443, "https://forge.example.com:8443/"},
		{named, 80, "https://forge.example.com:80/"},
		{named, 8222, "https://forge.example.com:8222/"},
		{nameless, 80, "http://host/"},
		{nameless, 8222, "http://host:8222/"},
		{nameless, 443, "http://host:443/"},
	} {
		host := "forge.example.com"
		if !tc.m.Named() {
			host = "host"
		}
		if got := tc.m.WebURL(host, tc.port); got != tc.want {
			t.Errorf("WebURL(%q, %d) = %q, want %q", host, tc.port, got, tc.want)
		}
	}
}

// An IPv6 address arrives already bracketed (deploy.NormalizeAddress), and
// a port has to sit outside those brackets or the URL is unparseable — the
// case a scheme-default port would have hidden.
func TestWebURLPutsThePortOutsideABracketedIPv6Literal(t *testing.T) {
	m := &Manifest{}
	if got, want := m.PublicURLAt("[fd00::1]"), "http://[fd00::1]:8222/"; got != want {
		t.Errorf("PublicURLAt = %q, want %q", got, want)
	}
}

// Moving a named bundle's published port has two possible meanings —
// clients follow it, or a proxy on 443 forwards to it — and Farrier cannot
// see which is true. Refusing is the whole point: deploying on a guess
// would bring up a healthy forge whose every rendered URL is wrong.
func TestNamedBundleOnANonStandardPortMustSayWhereClientsConnect(t *testing.T) {
	m := &Manifest{Domain: "forge.example.com", WebPort: 8443}

	err := m.ValidateWebPorts()
	if err == nil {
		t.Fatal("ValidateWebPorts: want a refusal for a moved port with no public port, got nil")
	}
	if !strings.Contains(err.Error(), "publicWebPort") {
		t.Errorf("refusal = %q, want it to name the field that resolves it", err)
	}

	// And Validate carries the same refusal, so no path writes a manifest
	// that cannot be deployed.
	full := validManifestForWebPorts()
	full.WebPort = 8443
	if err := full.Validate(); err == nil {
		t.Fatal("Validate: want a refusal for a moved port with no public port, got nil")
	}

	// Either answer resolves it — the moved port when nothing fronts the
	// instance, 443 when something does.
	for _, public := range []int{8443, DefaultNamedWebPort} {
		full.PublicWebPort = public
		if err := full.Validate(); err != nil {
			t.Errorf("Validate with publicWebPort %d: %v", public, err)
		}
	}
}

// A nameless bundle is exempt. Its default published port is already
// non-standard, nothing fronts a plain-HTTP instance on a trusted network
// (UP-006), and a second field to confirm the first would be a question
// with one possible answer.
func TestNamelessBundleNeedsNoPublicPortOnANonStandardPort(t *testing.T) {
	m := &Manifest{WebPort: 9000}
	if err := m.ValidateWebPorts(); err != nil {
		t.Errorf("ValidateWebPorts on a nameless bundle: %v", err)
	}
	if got, want := m.PublicURLAt("box"), "http://box:9000/"; got != want {
		t.Errorf("PublicURLAt = %q, want %q", got, want)
	}
}

// A manifest written before these fields existed carries neither, and must
// keep resolving to the same behavior its tier gets by default.
func TestWebPortsDefaultOnAManifestThatDeclaresNone(t *testing.T) {
	named := &Manifest{Domain: "forge.example.com"}
	if got := named.WebPortOrDefault(); got != DefaultNamedWebPort {
		t.Errorf("named WebPortOrDefault() = %d, want %d", got, DefaultNamedWebPort)
	}
	if got := named.PublicWebPortOrDefault(); got != DefaultNamedWebPort {
		t.Errorf("named PublicWebPortOrDefault() = %d, want %d", got, DefaultNamedWebPort)
	}

	nameless := &Manifest{}
	if got := nameless.WebPortOrDefault(); got != DefaultNamelessWebPort {
		t.Errorf("nameless WebPortOrDefault() = %d, want %d", got, DefaultNamelessWebPort)
	}
	if DefaultNamelessWebPort == 80 {
		t.Error("the nameless default is 80 again; it is deliberately not, because 80 is the port a developer's own machine is most likely to have taken")
	}
}

// The ports are bundle identity, like the git-over-SSH port beside them: a
// bundle copied to another machine has to come up on the same endpoints, or
// restore and promote deliver an instance clients cannot reach.
func TestWebPortsSurviveSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	b := &Bundle{Manifest: *validManifestForWebPorts(), Compose: map[string][]byte{"docker-compose.yml": []byte("services: {}\n")}}
	b.Manifest.WebPort = 8443
	b.Manifest.PublicWebPort = 443
	if err := b.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := loaded.Manifest.WebPortOrDefault(); got != 8443 {
		t.Errorf("WebPortOrDefault() = %d, want 8443", got)
	}
	if got := loaded.Manifest.PublicURL(); got != "https://forge.example.com/" {
		t.Errorf("PublicURL() = %q, want the fronted spelling with no port", got)
	}
}

// An impossible port is rejected at the front door rather than at the point
// Docker refuses the mapping.
func TestValidateWebPortRejectsAnImpossiblePort(t *testing.T) {
	for _, port := range []int{-1, 70000} {
		if err := ValidateWebPort(port); err == nil {
			t.Errorf("ValidateWebPort(%d) = nil, want an error", port)
		}
	}
	if err := ValidateWebPort(0); err != nil {
		t.Errorf("ValidateWebPort(0) = %v, want nil — zero is unset", err)
	}
}

func validManifestForWebPorts() *Manifest {
	return NewManifest("forge.example.com", map[string]string{
		"forgejo": "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("a", 64),
	}, DriverConfig{
		Keystore: DriverRef{Driver: "file"},
		Blob:     DriverRef{Driver: "local"},
	}, ACMEConfig{DNSProvider: "manual"})
}
