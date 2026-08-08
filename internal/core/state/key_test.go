package state

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

type fakeKeyDriver struct {
	values map[string]string
	errFor string
}

func (d fakeKeyDriver) Resolve(ctx context.Context, keyName string) (keystore.Secret, error) {
	if keyName == d.errFor {
		return keystore.Secret{}, errors.New("resolve failed")
	}
	v, ok := d.values[keyName]
	if !ok {
		return keystore.Secret{}, errors.New("no such key")
	}
	return keystore.NewSecret(v), nil
}

func testKeyDriver() fakeKeyDriver {
	return fakeKeyDriver{values: map[string]string{
		forge.KeySecretKey:     "sk-value",
		forge.KeyInternalToken: "it-value",
		forge.KeyLFSJWTSecret:  "lfs-value",
		KeyTLSCertificate:      "cert-value",
		KeyTLSPrivateKey:       "tls-key-value",
		KeySSHHostKey:          "ssh-host-key-value",
		KeySSHHostKeyPublic:    "ssh-host-key-public-value",
	}}
}

func TestKeystoreKeyExporterNamesReturnsFixedSet(t *testing.T) {
	exporter := &KeystoreKeyExporter{Driver: testKeyDriver()}

	names := exporter.Names()
	if len(names) != len(keyNames) {
		t.Fatalf("Names() returned %d names, want %d", len(names), len(keyNames))
	}

	got := append([]string(nil), names...)
	want := append([]string(nil), keyNames...)
	sort.Strings(got)
	sort.Strings(want)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
}

func TestKeystoreKeyExporterNamesReturnsACopy(t *testing.T) {
	exporter := &KeystoreKeyExporter{Driver: testKeyDriver()}

	names := exporter.Names()
	names[0] = "tampered"

	if keyNames[0] == "tampered" {
		t.Fatal("mutating the returned slice mutated the package's fixed key set")
	}
}

func TestKeystoreKeyExporterResolvesEveryName(t *testing.T) {
	exporter := &KeystoreKeyExporter{Driver: testKeyDriver()}
	ctx := context.Background()

	for _, name := range exporter.Names() {
		secret, err := exporter.Resolve(ctx, name)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", name, err)
		}
		want := testKeyDriver().values[name]
		if secret.Reveal() != want {
			t.Fatalf("Resolve(%s).Reveal() = %q, want %q", name, secret.Reveal(), want)
		}
	}
}

func TestKeystoreKeyExporterResolveRequiresDriver(t *testing.T) {
	exporter := &KeystoreKeyExporter{}
	if _, err := exporter.Resolve(context.Background(), forge.KeySecretKey); err == nil {
		t.Fatal("Resolve with nil driver: want error, got nil")
	}
}

func TestKeystoreKeyExporterResolveRequiresName(t *testing.T) {
	exporter := &KeystoreKeyExporter{Driver: testKeyDriver()}
	if _, err := exporter.Resolve(context.Background(), ""); err == nil {
		t.Fatal("Resolve(\"\"): want error, got nil")
	}
}

func TestKeystoreKeyExporterResolvePropagatesDriverError(t *testing.T) {
	driver := testKeyDriver()
	driver.errFor = forge.KeySecretKey
	exporter := &KeystoreKeyExporter{Driver: driver}

	if _, err := exporter.Resolve(context.Background(), forge.KeySecretKey); err == nil {
		t.Fatal("Resolve with failing driver: want error, got nil")
	}
}

func TestKeystoreKeyExporterSatisfiesKeyExporter(t *testing.T) {
	var _ KeyExporter = &KeystoreKeyExporter{Driver: testKeyDriver()}
}
