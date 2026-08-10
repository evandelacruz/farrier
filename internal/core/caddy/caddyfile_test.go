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

func TestRenderPushHoldCaddyfileRejectsPushesAndProxiesEverythingElse(t *testing.T) {
	out, err := RenderPushHoldCaddyfile("forge.example.com", "forgejo:3000", "backup in progress")
	if err != nil {
		t.Fatalf("RenderPushHoldCaddyfile: %v", err)
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
	if !strings.Contains(got, "method POST") || !strings.Contains(got, "path */git-receive-pack") {
		t.Errorf("caddyfile missing a matcher for the git push endpoint: %s", got)
	}
	if !strings.Contains(got, "path */info/refs") || !strings.Contains(got, "query service=git-receive-pack") {
		t.Errorf("caddyfile missing a matcher for the git push info/refs endpoint: %s", got)
	}
	if !strings.Contains(got, `respond "backup in progress" 503`) {
		t.Errorf("caddyfile missing the rejection response: %s", got)
	}
}

func TestRenderPushHoldCaddyfileRejectsEmptyDomain(t *testing.T) {
	if _, err := RenderPushHoldCaddyfile("", "forgejo:3000", "backup in progress"); err == nil {
		t.Fatal("RenderPushHoldCaddyfile: want error for empty domain, got nil")
	}
}

func TestRenderPushHoldCaddyfileRejectsEmptyUpstream(t *testing.T) {
	if _, err := RenderPushHoldCaddyfile("forge.example.com", "", "backup in progress"); err == nil {
		t.Fatal("RenderPushHoldCaddyfile: want error for empty upstream, got nil")
	}
}

func TestRenderPushHoldCaddyfileRejectsEmptyMessage(t *testing.T) {
	if _, err := RenderPushHoldCaddyfile("forge.example.com", "forgejo:3000", ""); err == nil {
		t.Fatal("RenderPushHoldCaddyfile: want error for empty message, got nil")
	}
}

// UP-006: a nameless bundle has no domain to name a certificate for, so
// Caddy serves the operator's address in plain HTTP — and must not fall
// into automatic HTTPS, which would make it the second ACME client this
// package exists to rule out.
func TestRenderPlainHTTPCaddyfileServesTheAddressWithoutTLS(t *testing.T) {
	out, err := RenderPlainHTTPCaddyfile(" box.tail1234.ts.net ", "forgejo:3000")
	if err != nil {
		t.Fatalf("RenderPlainHTTPCaddyfile: %v", err)
	}
	got := string(out)

	if !strings.Contains(got, "http://box.tail1234.ts.net {") {
		t.Errorf("caddyfile does not pin the site to plain HTTP: %s", got)
	}
	if !strings.Contains(got, "reverse_proxy forgejo:3000") {
		t.Errorf("caddyfile missing reverse_proxy to upstream: %s", got)
	}
	if strings.Contains(got, "tls") {
		t.Errorf("caddyfile carries a tls directive: %s", got)
	}
	if strings.Contains(got, CertPath) || strings.Contains(got, KeyPath) {
		t.Errorf("caddyfile references certificate files that a nameless bundle does not have: %s", got)
	}
}

// An IPv6 address arrives bracketed and stays that way: Caddy's site
// address is a URL authority like every other place it lands.
func TestRenderPlainHTTPCaddyfileAcceptsABracketedIPv6Address(t *testing.T) {
	out, err := RenderPlainHTTPCaddyfile("[fd00::1]", "forgejo:3000")
	if err != nil {
		t.Fatalf("RenderPlainHTTPCaddyfile: %v", err)
	}
	if !strings.Contains(string(out), "http://[fd00::1] {") {
		t.Errorf("caddyfile does not serve the bracketed literal: %s", out)
	}
}

func TestRenderPlainHTTPCaddyfileRejectsEmptyAddressOrUpstream(t *testing.T) {
	if _, err := RenderPlainHTTPCaddyfile("", "forgejo:3000"); err == nil {
		t.Fatal("RenderPlainHTTPCaddyfile: want error for empty address, got nil")
	}
	if _, err := RenderPlainHTTPCaddyfile("box.local", ""); err == nil {
		t.Fatal("RenderPlainHTTPCaddyfile: want error for empty upstream, got nil")
	}
}
