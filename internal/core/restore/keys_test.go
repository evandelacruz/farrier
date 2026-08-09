package restore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// buildKeysSnapshot writes one keys/<name> file per keyNames() into a fresh
// plain snapshot directory, holding values[name], and returns the directory
// alongside a *backup.Manifest describing exactly those components — enough
// for installKeys on its own, without going through fetch/decrypt/verify.
func buildKeysSnapshot(t *testing.T, values map[string]string) (string, *backup.Manifest) {
	t.Helper()
	dir := t.TempDir()
	keysDir := filepath.Join(dir, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		t.Fatalf("mkdir keys dir: %v", err)
	}

	manifest := &backup.Manifest{}
	for _, name := range keyNames() {
		if err := os.WriteFile(filepath.Join(keysDir, name), []byte(values[name]), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		manifest.Components = append(manifest.Components, backup.Component{
			Kind: bundle.StateKindKeys,
			Name: name,
			Path: "keys/" + name,
		})
	}
	return dir, manifest
}

// readOnlyKeystoreDriver satisfies keystore.Driver but not keystore.Writer,
// standing in for a target keystore driver with no way to persist anything
// (KEY-002's command driver, in production).
type readOnlyKeystoreDriver struct {
	values map[string]string
}

func (d readOnlyKeystoreDriver) Resolve(ctx context.Context, name string) (keystore.Secret, error) {
	v, ok := d.values[name]
	if !ok {
		return keystore.Secret{}, keystore.ErrNotFound
	}
	return keystore.NewSecret(v), nil
}

var _ keystore.Driver = readOnlyKeystoreDriver{}

func TestInstallKeysWritesEveryNameIntoAnEmptyKeystore(t *testing.T) {
	plainDir, manifest := buildKeysSnapshot(t, testKeyValues())
	target := &fakeKeystoreDriver{}
	job := events.NewJob()

	if err := installKeys(context.Background(), job, plainDir, manifest, target); err != nil {
		t.Fatalf("installKeys: %v", err)
	}

	values := testKeyValues()
	for _, name := range keyNames() {
		got, err := target.Resolve(context.Background(), name)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		if got.Reveal() != values[name] {
			t.Errorf("keystore[%s] = %q, want %q", name, got.Reveal(), values[name])
		}
	}
	if len(target.stores) != len(keyNames()) {
		t.Errorf("stored %d key(s), want %d: %v", len(target.stores), len(keyNames()), target.stores)
	}
}

func TestInstallKeysSkipsAlreadyMatchingValues(t *testing.T) {
	values := testKeyValues()
	plainDir, manifest := buildKeysSnapshot(t, values)

	target := &fakeKeystoreDriver{values: map[string]string{}}
	for name, v := range values {
		target.values[name] = v
	}

	job := events.NewJob()
	if err := installKeys(context.Background(), job, plainDir, manifest, target); err != nil {
		t.Fatalf("installKeys: %v", err)
	}
	if len(target.stores) != 0 {
		t.Errorf("installKeys stored %v, want no writes when every value already matches", target.stores)
	}
}

func TestInstallKeysRefusesOnConflictingValue(t *testing.T) {
	values := testKeyValues()
	plainDir, manifest := buildKeysSnapshot(t, values)

	target := &fakeKeystoreDriver{values: map[string]string{
		keyNames()[0]: "a-completely-different-value",
	}}

	job := events.NewJob()
	err := installKeys(context.Background(), job, plainDir, manifest, target)
	if err == nil {
		t.Fatal("installKeys: want error for a conflicting existing value, got nil")
	}
	if !strings.Contains(err.Error(), keyNames()[0]) {
		t.Errorf("error %q does not name the conflicting key %s", err, keyNames()[0])
	}
}

func TestInstallKeysWrapsStoreFailure(t *testing.T) {
	plainDir, manifest := buildKeysSnapshot(t, testKeyValues())
	target := &fakeKeystoreDriver{storeErr: context.DeadlineExceeded}

	job := events.NewJob()
	if err := installKeys(context.Background(), job, plainDir, manifest, target); err == nil {
		t.Fatal("installKeys: want error when Store fails, got nil")
	}
}

func TestInstallKeysRefusesWhenTargetCannotStore(t *testing.T) {
	plainDir, manifest := buildKeysSnapshot(t, testKeyValues())
	target := readOnlyKeystoreDriver{values: testKeyValues()}

	job := events.NewJob()
	if err := installKeys(context.Background(), job, plainDir, manifest, target); err == nil {
		t.Fatal("installKeys: want error for a keystore driver that cannot store, got nil")
	}
}
