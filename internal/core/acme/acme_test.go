package acme

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-acme/lego/v4/acme"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/platform/tester"
	"github.com/go-acme/lego/v4/platform/tester/servermock"
)

func TestConfigValidate(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	tests := []struct {
		name string
		cfg  Config
	}{
		{"missing domain", Config{AccountKey: key, DNSProvider: "manual"}},
		{"missing account key", Config{Domain: "forge.example.com", DNSProvider: "manual"}},
		{"missing dns provider", Config{Domain: "forge.example.com", AccountKey: key}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.cfg.validate(); err == nil {
				t.Fatalf("expected an error, got nil")
			}
		})
	}
}

func TestIssueUnknownDNSProvider(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	_, err := Issue(Config{
		Domain:      "forge.example.com",
		AccountKey:  key,
		DNSProvider: "not-a-real-provider",
	})
	if err == nil {
		t.Fatal("expected an error for an unrecognized DNS provider")
	}
	if !strings.Contains(err.Error(), "not-a-real-provider") {
		t.Errorf("error %q does not name the bad provider", err)
	}
}

func TestNeedsRenewal(t *testing.T) {
	notBefore := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter := notBefore.Add(90 * 24 * time.Hour)
	cert := &Certificate{NotBefore: notBefore, NotAfter: notAfter}

	twoThirds := notBefore.Add(60 * 24 * time.Hour)
	if NeedsRenewal(cert, twoThirds.Add(-time.Hour)) {
		t.Error("should not need renewal just before two-thirds of lifetime")
	}
	if !NeedsRenewal(cert, twoThirds.Add(time.Hour)) {
		t.Error("should need renewal just after two-thirds of lifetime")
	}
}

func TestExpiryWarning(t *testing.T) {
	notAfter := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	cert := &Certificate{NotBefore: notAfter.Add(-90 * 24 * time.Hour), NotAfter: notAfter}

	warn, remaining := ExpiryWarning(cert, notAfter.Add(-15*24*time.Hour))
	if warn {
		t.Error("should not warn 15 days out")
	}
	if remaining <= 14*24*time.Hour {
		t.Errorf("remaining = %s, want > 14 days", remaining)
	}

	warn, remaining = ExpiryWarning(cert, notAfter.Add(-13*24*time.Hour))
	if !warn {
		t.Error("should warn 13 days out")
	}
	if remaining > ExpiryWarningWindow {
		t.Errorf("remaining = %s, want <= 14 days", remaining)
	}
}

func TestParseCertificate(t *testing.T) {
	certPEM, notBefore, notAfter := generateTestCertChain(t, "forge.example.com")
	keyPEM := []byte("fake-key-pem")

	cert, err := ParseCertificate("forge.example.com", certPEM, keyPEM)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if cert.Domain != "forge.example.com" {
		t.Errorf("Domain = %q, want forge.example.com", cert.Domain)
	}
	if !cert.NotBefore.Equal(notBefore) || !cert.NotAfter.Equal(notAfter) {
		t.Errorf("NotBefore/NotAfter = %v/%v, want %v/%v", cert.NotBefore, cert.NotAfter, notBefore, notAfter)
	}
	if string(cert.Certificate) != string(certPEM) {
		t.Error("Certificate does not round-trip the input PEM")
	}
	if string(cert.PrivateKey) != string(keyPEM) {
		t.Error("PrivateKey does not round-trip the input PEM")
	}
}

func TestParseCertificateInvalidPEM(t *testing.T) {
	if _, err := ParseCertificate("forge.example.com", []byte("not a certificate"), nil); err == nil {
		t.Fatal("expected an error for invalid certificate PEM")
	}
}

func TestEnsureValid(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	fresh := &Certificate{NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour)}

	cert, renewed, err := EnsureValid(Config{}, fresh, now)
	if err != nil {
		t.Fatalf("EnsureValid: %v", err)
	}
	if renewed {
		t.Error("a fresh certificate should not trigger renewal")
	}
	if cert != fresh {
		t.Error("a fresh certificate should be returned unchanged")
	}

	due := &Certificate{NotBefore: now.Add(-89 * 24 * time.Hour), NotAfter: now.Add(time.Hour)}
	if _, _, err := EnsureValid(Config{}, due, now); err == nil {
		t.Fatal("expected a due certificate to trigger Issue, which should fail against an unconfigured Config")
	}
}

// fakeDNSProvider is a DNS-01 challenge.Provider that records every domain
// it's asked to present a record for, and can be made to fail on demand —
// standing in for a real DNS provider misconfiguration or outage so
// Issue's "fail with the reason" behavior (ACME-001) is exercised without
// a real DNS provider account.
type fakeDNSProvider struct {
	presentErr error

	mu        sync.Mutex
	presented []string
	cleanedUp []string
}

func (p *fakeDNSProvider) Present(domain, token, keyAuth string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.presented = append(p.presented, domain)
	return p.presentErr
}

func (p *fakeDNSProvider) CleanUp(domain, token, keyAuth string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleanedUp = append(p.cleanedUp, domain)
	return nil
}

// Timeout keeps dns01.Challenge.Solve's mandatory pre-poll sleep short:
// tests already skip the real propagation check via
// dns01.PropagationWait(0, true), but Solve sleeps one interval before
// running it regardless of provider type.
func (p *fakeDNSProvider) Timeout() (timeout, interval time.Duration) {
	return time.Second, time.Millisecond
}

// newFakeACMEServer builds a minimal in-process ACME server (RFC 8555)
// covering exactly the endpoints lego's client exercises during Obtain:
// account registration, order creation, authorization lookup, DNS-01
// challenge acceptance, order finalization, and certificate download. It
// always validates immediately, since exercising real CA validation is
// out of scope here — the behavior under test is Farrier's own wiring:
// provider resolution, error propagation, and certificate parsing.
func newFakeACMEServer(t *testing.T, domain string, certPEM []byte) *httptest.Server {
	t.Helper()

	locate := func(req *http.Request) string {
		return fmt.Sprintf("https://%v", req.Context().Value(http.LocalAddrContextKey))
	}

	return tester.MockACMEServer().
		Route("POST /account", http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			rw.Header().Set("Location", locate(req)+"/account/1")
			servermock.JSONEncode(acme.Account{Status: acme.StatusValid}).ServeHTTP(rw, req)
		})).
		Route("POST /newOrder", http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			rw.Header().Set("Location", locate(req)+"/order/1")
			servermock.JSONEncode(acme.Order{
				Status:         acme.StatusPending,
				Identifiers:    []acme.Identifier{{Type: "dns", Value: domain}},
				Authorizations: []string{locate(req) + "/authz/1"},
				Finalize:       locate(req) + "/finalize/1",
			}).ServeHTTP(rw, req)
		})).
		Route("POST /authz/1", http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			servermock.JSONEncode(acme.Authorization{
				Status:     acme.StatusPending,
				Identifier: acme.Identifier{Type: "dns", Value: domain},
				Challenges: []acme.Challenge{{
					Type:   string(challenge.DNS01),
					URL:    locate(req) + "/challenge/1",
					Token:  "test-token",
					Status: acme.StatusPending,
				}},
			}).ServeHTTP(rw, req)
		})).
		Route("POST /challenge/1", http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			servermock.JSONEncode(acme.Challenge{
				Type:   string(challenge.DNS01),
				URL:    locate(req) + "/challenge/1",
				Token:  "test-token",
				Status: acme.StatusValid,
			}).ServeHTTP(rw, req)
		})).
		Route("POST /finalize/1", http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			servermock.JSONEncode(acme.Order{
				Status:      acme.StatusValid,
				Identifiers: []acme.Identifier{{Type: "dns", Value: domain}},
				Certificate: locate(req) + "/cert/1",
			}).ServeHTTP(rw, req)
		})).
		Route("POST /cert/1", servermock.RawStringResponse(string(certPEM))).
		BuildHTTPS(t)
}

func TestIssueSuccess(t *testing.T) {
	domain := "forge.example.com"
	certPEM, notBefore, notAfter := generateTestCertChain(t, domain)

	server := newFakeACMEServer(t, domain, certPEM)
	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate account key: %v", err)
	}

	provider := &fakeDNSProvider{}
	cfg := Config{
		Domain:       domain,
		Email:        "ops@example.com",
		AccountKey:   accountKey,
		DNSProvider:  "manual",
		DirectoryURL: server.URL + "/dir",
		httpClient:   server.Client(),
	}

	cert, err := issue(cfg, provider, dns01.PropagationWait(0, true))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if cert.Domain != domain {
		t.Errorf("Domain = %q, want %q", cert.Domain, domain)
	}
	if !cert.NotBefore.Equal(notBefore) {
		t.Errorf("NotBefore = %v, want %v", cert.NotBefore, notBefore)
	}
	if !cert.NotAfter.Equal(notAfter) {
		t.Errorf("NotAfter = %v, want %v", cert.NotAfter, notAfter)
	}
	if len(provider.presented) != 1 || provider.presented[0] != domain {
		t.Errorf("presented = %v, want one entry for %q", provider.presented, domain)
	}
	if len(provider.cleanedUp) != 1 {
		t.Errorf("cleanedUp = %v, want one entry", provider.cleanedUp)
	}
}

func TestIssueChallengeFailure(t *testing.T) {
	domain := "forge.example.com"
	certPEM, _, _ := generateTestCertChain(t, domain)

	server := newFakeACMEServer(t, domain, certPEM)
	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate account key: %v", err)
	}

	presentErr := errors.New("could not reach DNS provider API")
	provider := &fakeDNSProvider{presentErr: presentErr}
	cfg := Config{
		Domain:       domain,
		Email:        "ops@example.com",
		AccountKey:   accountKey,
		DNSProvider:  "manual",
		DirectoryURL: server.URL + "/dir",
		httpClient:   server.Client(),
	}

	_, err = issue(cfg, provider, dns01.PropagationWait(0, true))
	if err == nil {
		t.Fatal("expected issue to fail when the DNS-01 provider can't present the challenge")
	}
	if !strings.Contains(err.Error(), presentErr.Error()) {
		t.Errorf("error %q does not carry the underlying reason %q", err, presentErr)
	}
	if !strings.Contains(err.Error(), domain) {
		t.Errorf("error %q does not name the domain", err)
	}
}

// generateTestCertChain builds a minimal self-signed issuer and a leaf
// certificate for domain, signed by that issuer, and returns the PEM
// bundle (leaf then issuer, matching lego's Bundle=true convention) plus
// the leaf's validity window for assertions.
func generateTestCertChain(t *testing.T, domain string) (bundlePEM []byte, notBefore, notAfter time.Time) {
	t.Helper()

	issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate issuer key: %v", err)
	}
	issuerTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test Issuer"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	issuerDER, err := x509.CreateCertificate(rand.Reader, issuerTemplate, issuerTemplate, &issuerKey.PublicKey, issuerKey)
	if err != nil {
		t.Fatalf("create issuer certificate: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	notBefore = time.Now().Add(-time.Minute).Truncate(time.Second)
	notAfter = notBefore.Add(90 * 24 * time.Hour)
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, issuerTemplate, &leafKey.PublicKey, issuerKey)
	if err != nil {
		t.Fatalf("create leaf certificate: %v", err)
	}

	var buf strings.Builder
	pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: issuerDER})

	return []byte(buf.String()), notBefore, notAfter
}
