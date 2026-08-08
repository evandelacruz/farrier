package deploy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"path"
	"time"

	"github.com/evandelacruz/farrier/internal/core/acme"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/caddy"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/state"
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

// CertIssuer returns a TLS certificate for cfg.Domain valid at now: existing
// unchanged if it isn't yet due for renewal, or freshly issued via ACME
// DNS-01 otherwise (renewed reports which). Lets tests substitute a fake
// for acme.EnsureValid's real ACME server and DNS provider calls. Satisfied
// by acmeCertIssuer, the production default.
type CertIssuer interface {
	EnsureValid(cfg acme.Config, existing *acme.Certificate, now time.Time) (cert *acme.Certificate, renewed bool, err error)
}

// acmeCertIssuer is the production CertIssuer: it calls acme.EnsureValid
// directly (ACME-002), the renewal-aware entry point that only reaches the
// ACME server — acme.Issue, the same one initialize's zone-control proof
// uses — when existing is nil or actually due for renewal.
type acmeCertIssuer struct{}

func (acmeCertIssuer) EnsureValid(cfg acme.Config, existing *acme.Certificate, now time.Time) (*acme.Certificate, bool, error) {
	return acme.EnsureValid(cfg, existing, now)
}

func issuerOrDefault(i CertIssuer) CertIssuer {
	if i != nil {
		return i
	}
	return acmeCertIssuer{}
}

// configureTLS resolves the certificate init already persisted as bundle
// key material (INIT-003, state.KeyTLSCertificate/state.KeyTLSPrivateKey) and hands it
// to the renewal-aware acme.EnsureValid (ACME-002, via issuer) rather than
// issuing a fresh certificate unconditionally: an ordinary re-run reuses
// the persisted certificate untouched, so it never reaches the ACME server
// and never risks Let's Encrypt's duplicate-certificate rate limit
// (UP-003). It renders Caddy's config using whatever certificate results,
// ships both to host, and returns compose with Caddy's bind mounts and its
// published HTTPS port added (UP-002). renewed reports whether issuer
// actually issued a fresh certificate — on that branch the renewed
// certificate is also persisted back to the keystore (ACME-002), through
// the same driver Store the rest of this function reads from: the TLS
// certificate and its private key are the one piece of key material the
// keystore's rotation guard (keystore.Rotates) lets overwrite, so this
// call succeeds where storing any other key material a second time would
// refuse.
//
// The ACME account key is generated fresh whenever issuance does happen,
// the same way initialize's zone-control proof generates one for its own
// — registering an account carries none of Let's Encrypt's issuance rate
// limits, so that part is harmless to repeat.
func configureTLS(ctx context.Context, host Host, b *bundle.Bundle, remoteDir string, compose map[string][]byte, issuer CertIssuer) (map[string][]byte, bool, error) {
	driver, err := keystore.New(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		return nil, false, fmt.Errorf("keystore driver: %w", err)
	}
	existing, err := resolvePersistedCertificate(ctx, driver, b.Manifest.Domain)
	if err != nil {
		return nil, false, err
	}

	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, false, fmt.Errorf("generate acme account key: %w", err)
	}

	cert, renewed, err := issuer.EnsureValid(acme.Config{
		Domain:      b.Manifest.Domain,
		Email:       b.Manifest.ACME.Email,
		AccountKey:  accountKey,
		DNSProvider: b.Manifest.ACME.DNSProvider,
	}, existing, time.Now())
	if err != nil {
		return nil, false, fmt.Errorf("issue certificate for %s: %w", b.Manifest.Domain, err)
	}
	if renewed {
		if err := persistRenewedCertificate(ctx, driver, cert); err != nil {
			return nil, false, err
		}
	}

	upstream := fmt.Sprintf("%s:%d", forge.Service, forge.HTTPPort)
	caddyfile, err := caddy.RenderCaddyfile(b.Manifest.Domain, upstream)
	if err != nil {
		return nil, false, fmt.Errorf("render caddyfile: %w", err)
	}

	caddyfilePath := path.Join(remoteDir, caddyConfigDir, caddyfileFilename)
	certPath := path.Join(remoteDir, caddyConfigDir, certFilename)
	keyPath := path.Join(remoteDir, caddyConfigDir, keyFilename)

	if err := host.WriteFile(ctx, caddyfilePath, caddyfile, 0o644); err != nil {
		return nil, false, fmt.Errorf("ship caddyfile: %w", err)
	}
	if err := host.WriteFile(ctx, certPath, cert.Certificate, 0o644); err != nil {
		return nil, false, fmt.Errorf("ship certificate: %w", err)
	}
	if err := host.WriteFile(ctx, keyPath, cert.PrivateKey, 0o600); err != nil {
		return nil, false, fmt.Errorf("ship private key: %w", err)
	}

	compose, err = orchestrate.WithBindMount(compose, caddy.Service, caddyfilePath, caddy.ConfigPath)
	if err != nil {
		return nil, false, fmt.Errorf("mount caddyfile: %w", err)
	}
	compose, err = orchestrate.WithBindMount(compose, caddy.Service, certPath, caddy.CertPath)
	if err != nil {
		return nil, false, fmt.Errorf("mount certificate: %w", err)
	}
	compose, err = orchestrate.WithBindMount(compose, caddy.Service, keyPath, caddy.KeyPath)
	if err != nil {
		return nil, false, fmt.Errorf("mount private key: %w", err)
	}
	compose, err = orchestrate.WithPorts(compose, caddy.Service, httpsPort, httpsPort)
	if err != nil {
		return nil, false, fmt.Errorf("publish https port: %w", err)
	}
	return compose, renewed, nil
}

// persistRenewedCertificate writes cert back to the keystore under
// state.KeyTLSCertificate/state.KeyTLSPrivateKey (ACME-002), so the
// renewal EnsureValid just decided actually takes effect: without this,
// the fresh certificate would serve this one deploy and then be
// forgotten, and the next `up` would find the old one still due and renew
// again. Every bundle's keystore target was already required to implement
// keystore.Writer at init time (initialize.Run) — the same target is used
// here, so this only fails if that target changed underneath the bundle
// since init.
func persistRenewedCertificate(ctx context.Context, driver keystore.Driver, cert *acme.Certificate) error {
	writer, ok := driver.(keystore.Writer)
	if !ok {
		return fmt.Errorf("persist renewed certificate: keystore driver cannot store key material")
	}
	if err := writer.Store(ctx, state.KeyTLSCertificate, keystore.NewSecret(string(cert.Certificate))); err != nil {
		return fmt.Errorf("persist renewed %s: %w", state.KeyTLSCertificate, err)
	}
	if err := writer.Store(ctx, state.KeyTLSPrivateKey, keystore.NewSecret(string(cert.PrivateKey))); err != nil {
		return fmt.Errorf("persist renewed %s: %w", state.KeyTLSPrivateKey, err)
	}
	return nil
}

// resolvePersistedCertificate resolves the TLS certificate and private key
// init persisted for domain (INIT-003) and parses them into an
// acme.Certificate, so EnsureValid can decide whether it's still due for
// renewal. Every bundle carries this key material from init onward, so a
// resolve failure here is a broken keystore, not an expected first-run
// case — it fails the same way configureForge does for the three Forgejo
// secrets, rather than treating it as "nothing persisted yet."
func resolvePersistedCertificate(ctx context.Context, driver keystore.Driver, domain string) (*acme.Certificate, error) {
	certSecret, err := driver.Resolve(ctx, state.KeyTLSCertificate)
	if err != nil {
		return nil, fmt.Errorf("resolve persisted %s: %w", state.KeyTLSCertificate, err)
	}
	keySecret, err := driver.Resolve(ctx, state.KeyTLSPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("resolve persisted %s: %w", state.KeyTLSPrivateKey, err)
	}
	cert, err := acme.ParseCertificate(domain, []byte(certSecret.Reveal()), []byte(keySecret.Reveal()))
	if err != nil {
		return nil, fmt.Errorf("parse persisted certificate: %w", err)
	}
	return cert, nil
}
