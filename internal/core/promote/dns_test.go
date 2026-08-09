package promote

import (
	"context"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/dns"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// fakeDNSKeystore is a minimal keystore.Driver a test seeds with fixed
// secrets — ResolveDNSDriver's own source of the DNS drivers' credentials.
type fakeDNSKeystore struct {
	values map[string]string
}

func (f *fakeDNSKeystore) Resolve(ctx context.Context, name string) (keystore.Secret, error) {
	v, ok := f.values[name]
	if !ok {
		return keystore.Secret{}, keystore.ErrNotFound
	}
	return keystore.NewSecret(v), nil
}

func TestResolveDNSDriverEmptyReturnsPrintDriver(t *testing.T) {
	job := events.NewJob()
	d, err := ResolveDNSDriver(context.Background(), job, bundle.DriverRef{}, &fakeDNSKeystore{})
	if err != nil {
		t.Fatalf("ResolveDNSDriver: %v", err)
	}
	if _, ok := d.(*dns.PrintDriver); !ok {
		t.Fatalf("ResolveDNSDriver with no driver name = %T, want *dns.PrintDriver", d)
	}
}

func TestResolveDNSDriverCloudflare(t *testing.T) {
	ks := &fakeDNSKeystore{values: map[string]string{keyCloudflareAPIToken: "token-value"}}
	ref := bundle.DriverRef{Driver: "cloudflare", Config: map[string]any{"zoneId": "zone-123"}}

	d, err := ResolveDNSDriver(context.Background(), events.NewJob(), ref, ks)
	if err != nil {
		t.Fatalf("ResolveDNSDriver: %v", err)
	}
	if _, ok := d.(*dns.CloudflareDriver); !ok {
		t.Fatalf("ResolveDNSDriver(cloudflare) = %T, want *dns.CloudflareDriver", d)
	}
}

func TestResolveDNSDriverCloudflareMissingZoneID(t *testing.T) {
	ks := &fakeDNSKeystore{values: map[string]string{keyCloudflareAPIToken: "token-value"}}
	ref := bundle.DriverRef{Driver: "cloudflare", Config: map[string]any{}}

	if _, err := ResolveDNSDriver(context.Background(), events.NewJob(), ref, ks); err == nil {
		t.Fatal("ResolveDNSDriver: want error for missing config.zoneId, got nil")
	}
}

func TestResolveDNSDriverCloudflareMissingSecret(t *testing.T) {
	ks := &fakeDNSKeystore{}
	ref := bundle.DriverRef{Driver: "cloudflare", Config: map[string]any{"zoneId": "zone-123"}}

	if _, err := ResolveDNSDriver(context.Background(), events.NewJob(), ref, ks); err == nil {
		t.Fatal("ResolveDNSDriver: want error when the api token secret isn't in the keystore, got nil")
	}
}

func TestResolveDNSDriverRFC2136(t *testing.T) {
	ks := &fakeDNSKeystore{values: map[string]string{keyRFC2136TSIGSecret: "secret-value"}}
	ref := bundle.DriverRef{Driver: "rfc2136", Config: map[string]any{
		"server":  "ns1.example.com",
		"zone":    "example.com.",
		"keyName": "farrier-key",
	}}

	d, err := ResolveDNSDriver(context.Background(), events.NewJob(), ref, ks)
	if err != nil {
		t.Fatalf("ResolveDNSDriver: %v", err)
	}
	if _, ok := d.(*dns.RFC2136Driver); !ok {
		t.Fatalf("ResolveDNSDriver(rfc2136) = %T, want *dns.RFC2136Driver", d)
	}
}

func TestResolveDNSDriverRFC2136MissingConfig(t *testing.T) {
	ks := &fakeDNSKeystore{values: map[string]string{keyRFC2136TSIGSecret: "secret-value"}}
	tests := []map[string]any{
		{"zone": "example.com.", "keyName": "farrier-key"},
		{"server": "ns1.example.com", "keyName": "farrier-key"},
		{"server": "ns1.example.com", "zone": "example.com."},
	}
	for _, config := range tests {
		ref := bundle.DriverRef{Driver: "rfc2136", Config: config}
		if _, err := ResolveDNSDriver(context.Background(), events.NewJob(), ref, ks); err == nil {
			t.Errorf("ResolveDNSDriver(rfc2136, config=%v): want error, got nil", config)
		}
	}
}

func TestResolveDNSDriverExecOutOfTree(t *testing.T) {
	ref := bundle.DriverRef{Driver: "route53", Config: map[string]any{"path": "/usr/local/bin/farrier-dns-route53"}}

	d, err := ResolveDNSDriver(context.Background(), events.NewJob(), ref, &fakeDNSKeystore{})
	if err != nil {
		t.Fatalf("ResolveDNSDriver: %v", err)
	}
	if _, ok := d.(*dns.ExecDriver); !ok {
		t.Fatalf("ResolveDNSDriver(route53) = %T, want *dns.ExecDriver", d)
	}
}

func TestResolveDNSDriverExecMissingPath(t *testing.T) {
	ref := bundle.DriverRef{Driver: "route53", Config: map[string]any{}}
	if _, err := ResolveDNSDriver(context.Background(), events.NewJob(), ref, &fakeDNSKeystore{}); err == nil {
		t.Fatal("ResolveDNSDriver: want error for an out-of-tree driver missing config.path, got nil")
	}
}
