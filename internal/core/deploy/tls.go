package deploy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"path"

	"github.com/evandelacruz/farrier/internal/core/acme"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/caddy"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// caddyConfigDir is the directory under RemoteDir Up writes Caddy's
// deploy-time configuration to: the rendered Caddyfile plus the ACME-issued
// certificate and key. Deploy-time content, not bundle content, the same
// split hostConfigDir draws for app.ini (KEY-003).
const caddyConfigDir = "caddy"

const (
	caddyfileFilename = "Caddyfile"
	certFilename      = "tls.crt"
	keyFilename       = "tls.key"
)

// httpsPort is the host and container port Caddy publishes for UP-002
// ("usable in a browser immediately").
const httpsPort = "443"

// CertIssuer issues a TLS certificate for cfg.Domain via ACME DNS-01,
// letting tests substitute a fake for acme.Issue's real ACME server and DNS
// provider calls. Satisfied by acmeCertIssuer, the production default.
type CertIssuer interface {
	Issue(cfg acme.Config) (*acme.Certificate, error)
}

// acmeCertIssuer is the production CertIssuer: it calls acme.Issue directly
// (ACME-001), the same entry point initialize's zone-control proof uses.
type acmeCertIssuer struct{}

func (acmeCertIssuer) Issue(cfg acme.Config) (*acme.Certificate, error) {
	return acme.Issue(cfg)
}

func issuerOrDefault(i CertIssuer) CertIssuer {
	if i != nil {
		return i
	}
	return acmeCertIssuer{}
}

// configureTLS issues a certificate for b's domain via ACME DNS-01, using
// the DNS-01 provider name b.Manifest.ACME carries from init's own
// zone-control proof (INIT-002), renders Caddy's config, ships both to
// host, and returns compose with Caddy's bind mounts and its published
// HTTPS port added (UP-002).
//
// The ACME account key is generated fresh for this issuance, the same way
// initialize's zone-control proof generates one for its own — registering
// an account carries none of Let's Encrypt's issuance rate limits, so that
// part is harmless to repeat. The certificate itself is also reissued on
// every call, though: init already persists one as durable bundle key
// material (INIT-003) and acme.EnsureValid (ACME-002) exists to decide
// against it whether a new one is actually due, but there is no path yet
// to persist a renewed certificate back to the keystore (see tech-spec.md
// "Known gap" under Deployment) — UP-003's re-run story is partial until
// that lands.
func configureTLS(ctx context.Context, host Host, b *bundle.Bundle, remoteDir string, compose map[string][]byte, issuer CertIssuer) (map[string][]byte, error) {
	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate acme account key: %w", err)
	}

	cert, err := issuer.Issue(acme.Config{
		Domain:      b.Manifest.Domain,
		Email:       b.Manifest.ACME.Email,
		AccountKey:  accountKey,
		DNSProvider: b.Manifest.ACME.DNSProvider,
	})
	if err != nil {
		return nil, fmt.Errorf("issue certificate for %s: %w", b.Manifest.Domain, err)
	}

	upstream := fmt.Sprintf("%s:%d", forge.Service, forge.HTTPPort)
	caddyfile, err := caddy.RenderCaddyfile(b.Manifest.Domain, upstream)
	if err != nil {
		return nil, fmt.Errorf("render caddyfile: %w", err)
	}

	caddyfilePath := path.Join(remoteDir, caddyConfigDir, caddyfileFilename)
	certPath := path.Join(remoteDir, caddyConfigDir, certFilename)
	keyPath := path.Join(remoteDir, caddyConfigDir, keyFilename)

	if err := host.WriteFile(ctx, caddyfilePath, caddyfile, 0o644); err != nil {
		return nil, fmt.Errorf("ship caddyfile: %w", err)
	}
	if err := host.WriteFile(ctx, certPath, cert.Certificate, 0o644); err != nil {
		return nil, fmt.Errorf("ship certificate: %w", err)
	}
	if err := host.WriteFile(ctx, keyPath, cert.PrivateKey, 0o600); err != nil {
		return nil, fmt.Errorf("ship private key: %w", err)
	}

	compose, err = orchestrate.WithBindMount(compose, caddy.Service, caddyfilePath, caddy.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("mount caddyfile: %w", err)
	}
	compose, err = orchestrate.WithBindMount(compose, caddy.Service, certPath, caddy.CertPath)
	if err != nil {
		return nil, fmt.Errorf("mount certificate: %w", err)
	}
	compose, err = orchestrate.WithBindMount(compose, caddy.Service, keyPath, caddy.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("mount private key: %w", err)
	}
	compose, err = orchestrate.WithPorts(compose, caddy.Service, httpsPort, httpsPort)
	if err != nil {
		return nil, fmt.Errorf("publish https port: %w", err)
	}
	return compose, nil
}
