// Package acme issues and renews TLS certificates via ACME DNS-01
// challenges, in-process, using lego (ACME-001, ACME-002).
//
// DNS-01 challenge solving is independent of the bundle's own DNS driver
// (internal/core/dns): lego resolves its DNS-01 provider from its own
// provider set, covering roughly one hundred DNS providers, each
// configured the way that provider documents — almost always its own
// environment variables. A caller resolves those credentials through the
// keystore and sets them before calling Issue; this package never touches
// the keystore or the bundle manifest directly.
package acme

import (
	"crypto"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"
	legodns "github.com/go-acme/lego/v4/providers/dns"
	"github.com/go-acme/lego/v4/registration"
)

// Config configures certificate issuance and renewal for one domain via
// ACME DNS-01 (ACME-001).
type Config struct {
	// Domain is the bundle domain the certificate is issued for.
	Domain string
	// Email is the contact address on the ACME account.
	Email string
	// AccountKey is the ACME account's private key. It is bundle key
	// material (KEY-003): callers resolve it through a keystore driver
	// and pass it in already resolved — this package never persists it.
	AccountKey crypto.PrivateKey
	// DNSProvider is a lego-recognized DNS-01 provider name (e.g.
	// "cloudflare", "rfc2136"). The named provider reads its own
	// credentials from the process environment; the caller sets them,
	// resolved through the keystore, before calling Issue.
	DNSProvider string
	// DirectoryURL is the ACME server's directory endpoint — the operator's
	// choice of CA, resolved through ResolveDirectoryURL and carried in the
	// bundle manifest so that whatever issued a certificate is also what
	// renews it. Empty defaults to ProductionDirectoryURL, which is what a
	// manifest written before the field existed carries.
	DirectoryURL string

	// httpClient overrides the HTTP client lego uses to reach the ACME
	// server. Only set by tests, to point at an in-process fake server.
	httpClient *http.Client
}

func (c Config) validate() error {
	if strings.TrimSpace(c.Domain) == "" {
		return errors.New("acme: domain is required")
	}
	if c.AccountKey == nil {
		return errors.New("acme: account key is required")
	}
	if strings.TrimSpace(c.DNSProvider) == "" {
		return errors.New("acme: dns provider is required")
	}
	return nil
}

// Certificate is an issued certificate: the full chain and private key,
// PEM-encoded and ready to hand to Caddy, plus the validity window
// NeedsRenewal and ExpiryWarning use to decide when it's due for renewal
// (ACME-002).
type Certificate struct {
	Domain            string
	Certificate       []byte
	PrivateKey        []byte
	IssuerCertificate []byte
	NotBefore         time.Time
	NotAfter          time.Time
}

// account adapts an already-resolved ACME account key to lego's
// registration.User.
type account struct {
	email string
	key   crypto.PrivateKey
	reg   *registration.Resource
}

func (a *account) GetEmail() string                        { return a.email }
func (a *account) GetRegistration() *registration.Resource { return a.reg }
func (a *account) GetPrivateKey() crypto.PrivateKey        { return a.key }

// Issue obtains a new certificate for cfg.Domain via ACME DNS-01,
// in-process (ACME-001). It registers the ACME account, resolves the
// named DNS-01 provider, and fails with the specific reason — an unknown
// provider name, a DNS-01 challenge the provider couldn't present, an
// authorization the CA rejected — wrapped with the domain for context.
func Issue(cfg Config) (*Certificate, error) {
	provider, err := legodns.NewDNSChallengeProviderByName(cfg.DNSProvider)
	if err != nil {
		return nil, fmt.Errorf("acme: dns-01 provider %q: %w", cfg.DNSProvider, err)
	}
	return issue(cfg, provider)
}

func issue(cfg Config, provider challenge.Provider, opts ...dns01.ChallengeOption) (*Certificate, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	if err := client.Challenge.SetDNS01Provider(provider, opts...); err != nil {
		return nil, fmt.Errorf("acme: configure dns-01: %w", err)
	}
	if _, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true}); err != nil {
		return nil, fmt.Errorf("acme: register account: %w", err)
	}

	res, err := client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: []string{cfg.Domain},
		Bundle:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("acme: issue certificate for %s: %w", cfg.Domain, err)
	}
	return newCertificate(res)
}

// directoryURL is the ACME server this config reaches: the caller's own,
// or Let's Encrypt production when it names none. Empty is what a manifest
// written before the bundle recorded a CA carries, so the default lives
// here, in one place, rather than in each caller that builds a Config.
func (c Config) directoryURL() string {
	if url := strings.TrimSpace(c.DirectoryURL); url != "" {
		return url
	}
	return ProductionDirectoryURL
}

func newClient(cfg Config) (*lego.Client, error) {
	legoCfg := lego.NewConfig(&account{email: cfg.Email, key: cfg.AccountKey})
	legoCfg.CADirURL = cfg.directoryURL()
	if cfg.httpClient != nil {
		legoCfg.HTTPClient = cfg.httpClient
	}

	client, err := lego.NewClient(legoCfg)
	if err != nil {
		return nil, fmt.Errorf("acme: create client: %w", err)
	}
	return client, nil
}

// ParseCertificate builds a Certificate from a PEM-encoded chain and
// private key previously returned by Issue and persisted elsewhere — a
// keystore, for instance — so a caller that resolved them back out can
// hand the result to NeedsRenewal or EnsureValid the same way it would a
// certificate Issue just returned.
func ParseCertificate(domain string, certPEM, privateKeyPEM []byte) (*Certificate, error) {
	leaf, err := certcrypto.ParsePEMCertificate(certPEM)
	if err != nil {
		return nil, fmt.Errorf("acme: parse certificate for %s: %w", domain, err)
	}
	return &Certificate{
		Domain:      domain,
		Certificate: certPEM,
		PrivateKey:  privateKeyPEM,
		NotBefore:   leaf.NotBefore,
		NotAfter:    leaf.NotAfter,
	}, nil
}

func newCertificate(res *certificate.Resource) (*Certificate, error) {
	leaf, err := certcrypto.ParsePEMCertificate(res.Certificate)
	if err != nil {
		return nil, fmt.Errorf("acme: parse issued certificate: %w", err)
	}
	return &Certificate{
		Domain:            res.Domain,
		Certificate:       res.Certificate,
		PrivateKey:        res.PrivateKey,
		IssuerCertificate: res.IssuerCertificate,
		NotBefore:         leaf.NotBefore,
		NotAfter:          leaf.NotAfter,
	}, nil
}
