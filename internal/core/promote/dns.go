package promote

import (
	"context"
	"fmt"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/dns"
	"github.com/evandelacruz/farrier/internal/core/driver"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// Keystore key names the shipped DNS drivers' secrets are resolved under —
// the same fixed-name convention forge.KeySecretKey and friends use for
// Forgejo's own secrets. A DNS DriverRef's Config (bundle.DriverConfig.DNS)
// carries only non-secret reference config — Cloudflare's zone ID, RFC
// 2136's server and zone — never the credential itself (dns package doc
// comment; the same split blob.S3Config draws for its secret access key).
const (
	keyCloudflareAPIToken = "dns_cloudflare_api_token"
	keyRFC2136TSIGSecret  = "dns_rfc2136_tsig_secret"
)

// ResolveDNSDriver builds the dns.Driver a promotion's DNS flip (FAIL-001,
// spec.md "Failover" step 4) applies through, from a bundle's optional DNS
// driver reference and its keystore driver: an empty ref.Driver returns
// dns.NewPrint(job) — DNS-003's "with no driver configured, print the
// exact record change instead of failing" — "cloudflare" and "rfc2136"
// build the two shipped drivers (DNS-001, DNS-002) with their secret
// resolved from keystoreDriver under the fixed key names above, and any
// other name is treated as an out-of-tree driver reached through the
// CORE-003 exec protocol — the same plugin posture keystore.New and
// blob.New already give their own driver types.
//
// cmd/farrier/promote.go and internal/api/promote.go both call this
// instead of duplicating the driver switch (CLAUDE.md "one core, thin
// skins").
func ResolveDNSDriver(ctx context.Context, job *events.Job, ref bundle.DriverRef, keystoreDriver keystore.Driver) (dns.Driver, error) {
	switch ref.Driver {
	case "":
		return dns.NewPrint(job), nil
	case "cloudflare":
		return resolveCloudflareDriver(ctx, ref.Config, keystoreDriver)
	case "rfc2136":
		return resolveRFC2136Driver(ctx, ref.Config, keystoreDriver)
	default:
		return resolveExecDNSDriver(ref.Driver, ref.Config)
	}
}

func resolveCloudflareDriver(ctx context.Context, config map[string]any, keystoreDriver keystore.Driver) (dns.Driver, error) {
	zoneID, err := dnsStringConfig(config, "zoneId")
	if err != nil {
		return nil, fmt.Errorf("promote: dns: cloudflare: %w", err)
	}
	token, err := resolveDNSSecret(ctx, keystoreDriver, keyCloudflareAPIToken)
	if err != nil {
		return nil, fmt.Errorf("promote: dns: cloudflare: %w", err)
	}
	d, err := dns.NewCloudflare(dns.CloudflareConfig{ZoneID: zoneID, APIToken: token})
	if err != nil {
		return nil, fmt.Errorf("promote: dns: cloudflare: %w", err)
	}
	return d, nil
}

func resolveRFC2136Driver(ctx context.Context, config map[string]any, keystoreDriver keystore.Driver) (dns.Driver, error) {
	server, err := dnsStringConfig(config, "server")
	if err != nil {
		return nil, fmt.Errorf("promote: dns: rfc2136: %w", err)
	}
	zone, err := dnsStringConfig(config, "zone")
	if err != nil {
		return nil, fmt.Errorf("promote: dns: rfc2136: %w", err)
	}
	keyName, err := dnsStringConfig(config, "keyName")
	if err != nil {
		return nil, fmt.Errorf("promote: dns: rfc2136: %w", err)
	}
	algorithm, _ := config["algorithm"].(string)

	secret, err := resolveDNSSecret(ctx, keystoreDriver, keyRFC2136TSIGSecret)
	if err != nil {
		return nil, fmt.Errorf("promote: dns: rfc2136: %w", err)
	}
	d, err := dns.NewRFC2136(dns.RFC2136Config{
		Server:    server,
		Zone:      zone,
		KeyName:   keyName,
		KeySecret: secret,
		Algorithm: algorithm,
	})
	if err != nil {
		return nil, fmt.Errorf("promote: dns: rfc2136: %w", err)
	}
	return d, nil
}

func resolveExecDNSDriver(driverName string, config map[string]any) (dns.Driver, error) {
	path, err := dnsStringConfig(config, "path")
	if err != nil {
		return nil, fmt.Errorf("promote: dns: %s: %w (unrecognized driver name, treated as an out-of-tree exec driver)", driverName, err)
	}

	var args []string
	if raw, ok := config["args"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("promote: dns: %s: config.args must be a list of strings", driverName)
		}
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("promote: dns: %s: config.args must be a list of strings", driverName)
			}
			args = append(args, s)
		}
	}

	return dns.NewExec(driver.Exec{Path: path, Args: args}), nil
}

func resolveDNSSecret(ctx context.Context, keystoreDriver keystore.Driver, keyName string) (string, error) {
	if keystoreDriver == nil {
		return "", fmt.Errorf("resolve %s: keystore driver is required", keyName)
	}
	secret, err := keystoreDriver.Resolve(ctx, keyName)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", keyName, err)
	}
	return secret.Reveal(), nil
}

// dnsStringConfig reads a required, non-empty string field out of a DNS
// driver's config map, naming the field in any error — the same helper
// keystore.New's and blob.New's own drivers use.
func dnsStringConfig(config map[string]any, field string) (string, error) {
	raw, ok := config[field]
	if !ok {
		return "", fmt.Errorf("config.%s is required", field)
	}
	s, ok := raw.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("config.%s must be a non-empty string", field)
	}
	return s, nil
}
