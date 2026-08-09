package orchestrate

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func networksOf(t *testing.T, files map[string][]byte, service string) map[string]any {
	t.Helper()
	for _, raw := range files {
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse compose: %v", err)
		}
		services, _ := doc["services"].(map[string]any)
		svc, ok := services[service].(map[string]any)
		if !ok {
			continue
		}
		networks, ok := svc["networks"].(map[string]any)
		if !ok {
			t.Fatalf("service %q networks are not in mapping form: %v", service, svc["networks"])
		}
		return networks
	}
	t.Fatalf("no file declares service %q", service)
	return nil
}

func aliasesOf(t *testing.T, networks map[string]any, network string) []string {
	t.Helper()
	options, ok := networks[network].(map[string]any)
	if !ok {
		t.Fatalf("network %q carries no options: %v", network, networks[network])
	}
	entries, _ := options["aliases"].([]any)
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.(string))
	}
	return out
}

func TestWithNetworkAliasAddsAnAliasOnEveryNetworkTheServiceJoins(t *testing.T) {
	files := map[string][]byte{
		"docker-compose.yml": []byte("services:\n  caddy:\n    image: caddy\n    networks: [farrier, other]\n"),
	}

	out, err := WithNetworkAlias(files, "caddy", "forge.example.com")
	if err != nil {
		t.Fatalf("WithNetworkAlias: %v", err)
	}

	networks := networksOf(t, out, "caddy")
	for _, network := range []string{"farrier", "other"} {
		aliases := aliasesOf(t, networks, network)
		if len(aliases) != 1 || aliases[0] != "forge.example.com" {
			t.Errorf("network %q aliases = %v, want [forge.example.com]", network, aliases)
		}
	}
}

// The rendered Compose a bundle carries lists networks by name; aliases
// only exist in the mapping form, and converting must not drop the
// membership itself.
func TestWithNetworkAliasKeepsRenderedMembership(t *testing.T) {
	rendered, err := Render(testManifest())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	out, err := WithNetworkAlias(rendered, "caddy", "forge.example.com")
	if err != nil {
		t.Fatalf("WithNetworkAlias: %v", err)
	}

	networks := networksOf(t, out, "caddy")
	if _, ok := networks[networkName]; !ok {
		t.Fatalf("caddy left the %s network: %v", networkName, networks)
	}
	if aliases := aliasesOf(t, networks, networkName); len(aliases) != 1 || aliases[0] != "forge.example.com" {
		t.Errorf("aliases = %v, want [forge.example.com]", aliases)
	}
}

func TestWithNetworkAliasAppendsToExistingAliases(t *testing.T) {
	files := map[string][]byte{
		"docker-compose.yml": []byte("services:\n  caddy:\n    image: caddy\n    networks:\n      farrier:\n        aliases: [first.example.com]\n"),
	}

	out, err := WithNetworkAlias(files, "caddy", "second.example.com")
	if err != nil {
		t.Fatalf("WithNetworkAlias: %v", err)
	}

	aliases := aliasesOf(t, networksOf(t, out, "caddy"), "farrier")
	if len(aliases) != 2 || aliases[0] != "first.example.com" || aliases[1] != "second.example.com" {
		t.Errorf("aliases = %v, want both the existing and the new one", aliases)
	}
}

func TestWithNetworkAliasLeavesInputAndOtherServicesAlone(t *testing.T) {
	source := "services:\n  caddy:\n    image: caddy\n    networks: [farrier]\n  forgejo:\n    image: forgejo\n    networks: [farrier]\n"
	files := map[string][]byte{"docker-compose.yml": []byte(source)}

	out, err := WithNetworkAlias(files, "caddy", "forge.example.com")
	if err != nil {
		t.Fatalf("WithNetworkAlias: %v", err)
	}

	if string(files["docker-compose.yml"]) != source {
		t.Error("WithNetworkAlias mutated its input")
	}

	var doc map[string]any
	if err := yaml.Unmarshal(out["docker-compose.yml"], &doc); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	services, _ := doc["services"].(map[string]any)
	forgejo, _ := services["forgejo"].(map[string]any)
	if _, aliased := forgejo["networks"].(map[string]any); aliased {
		t.Errorf("forgejo's networks were rewritten too: %v", forgejo["networks"])
	}
}

// A service with no networks key of its own is on Compose's implicit
// default network, and that is where its alias belongs — the service stays
// exactly where it was.
func TestWithNetworkAliasNamesTheImplicitDefaultNetwork(t *testing.T) {
	files := map[string][]byte{
		"docker-compose.yml": []byte("services:\n  caddy:\n    image: caddy\n"),
	}

	out, err := WithNetworkAlias(files, "caddy", "forge.example.com")
	if err != nil {
		t.Fatalf("WithNetworkAlias: %v", err)
	}

	aliases := aliasesOf(t, networksOf(t, out, "caddy"), defaultNetwork)
	if len(aliases) != 1 || aliases[0] != "forge.example.com" {
		t.Errorf("aliases on the default network = %v, want [forge.example.com]", aliases)
	}
}

func TestWithNetworkAliasRejectsWhatItCannotAlias(t *testing.T) {
	cases := map[string]struct {
		files   map[string][]byte
		service string
		alias   string
	}{
		"unknown service": {
			files:   map[string][]byte{"docker-compose.yml": []byte("services:\n  caddy:\n    image: caddy\n    networks: [farrier]\n")},
			service: "runner",
			alias:   "forge.example.com",
		},
		"empty alias": {
			files:   map[string][]byte{"docker-compose.yml": []byte("services:\n  caddy:\n    image: caddy\n    networks: [farrier]\n")},
			service: "caddy",
			alias:   "",
		},
	}
	for name, tc := range cases {
		if _, err := WithNetworkAlias(tc.files, tc.service, tc.alias); err == nil {
			t.Errorf("WithNetworkAlias(%s) succeeded, want error", name)
		} else if !strings.Contains(err.Error(), "orchestrate: with network alias") {
			t.Errorf("WithNetworkAlias(%s) error = %v, want it to name the operation", name, err)
		}
	}
}
