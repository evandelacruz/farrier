package forge

import (
	"context"
	"errors"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/keystore"
)

type fakeDriver struct {
	values map[string]string
	errFor string
}

func (d fakeDriver) Resolve(ctx context.Context, keyName string) (keystore.Secret, error) {
	if keyName == d.errFor {
		return keystore.Secret{}, errors.New("resolve failed")
	}
	v, ok := d.values[keyName]
	if !ok {
		return keystore.Secret{}, errors.New("no such key")
	}
	return keystore.NewSecret(v), nil
}

func testDriver() fakeDriver {
	return fakeDriver{values: map[string]string{
		KeySecretKey:     "sk-value",
		KeyInternalToken: "it-value",
		KeyLFSJWTSecret:  "lfs-value",
	}}
}

func TestResolveSecretsResolvesAllThree(t *testing.T) {
	secrets, err := ResolveSecrets(context.Background(), testDriver())
	if err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	if secrets.SecretKey != "sk-value" {
		t.Errorf("SecretKey = %q", secrets.SecretKey)
	}
	if secrets.InternalToken != "it-value" {
		t.Errorf("InternalToken = %q", secrets.InternalToken)
	}
	if secrets.LFSJWTSecret != "lfs-value" {
		t.Errorf("LFSJWTSecret = %q", secrets.LFSJWTSecret)
	}
}

func TestResolveSecretsFailsOnResolveError(t *testing.T) {
	d := testDriver()
	d.errFor = KeyLFSJWTSecret
	if _, err := ResolveSecrets(context.Background(), d); err == nil {
		t.Fatal("ResolveSecrets: want error when a key fails to resolve, got nil")
	}
}
