package caddy

import (
	"strings"
	"testing"
)

func TestRenderCaddyfileTerminatesTLSAndProxies(t *testing.T) {
	out, err := RenderCaddyfile("forge.example.com", "forgejo:3000")
	if err != nil {
		t.Fatalf("RenderCaddyfile: %v", err)
	}
	got := string(out)

	if !strings.HasPrefix(got, "forge.example.com {\n") {
		t.Errorf("caddyfile does not open the domain block: %s", got)
	}
	if !strings.Contains(got, "tls "+CertPath+" "+KeyPath) {
		t.Errorf("caddyfile missing explicit tls directive: %s", got)
	}
	if !strings.Contains(got, "reverse_proxy forgejo:3000") {
		t.Errorf("caddyfile missing reverse_proxy to upstream: %s", got)
	}
}

func TestRenderCaddyfileRejectsEmptyDomain(t *testing.T) {
	if _, err := RenderCaddyfile("", "forgejo:3000"); err == nil {
		t.Fatal("RenderCaddyfile: want error for empty domain, got nil")
	}
}

func TestRenderCaddyfileRejectsEmptyUpstream(t *testing.T) {
	if _, err := RenderCaddyfile("forge.example.com", ""); err == nil {
		t.Fatal("RenderCaddyfile: want error for empty upstream, got nil")
	}
}
