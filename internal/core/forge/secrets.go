package forge

import (
	"context"
	"fmt"

	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// Key names Secrets resolves from a keystore driver. This is bundle key
// material (spec.md "Identity" > "Key material"): generated once at init
// and carried through every backup and restore. ResolveSecrets only reads
// them by these names — it never generates or persists them.
const (
	KeySecretKey     = "forgejo_secret_key"
	KeyInternalToken = "forgejo_internal_token"
	KeyLFSJWTSecret  = "forgejo_lfs_jwt_secret"
)

// ResolveSecrets resolves Secrets' three fields from driver, by name
// (KeySecretKey, KeyInternalToken, KeyLFSJWTSecret) — the deploy-time step
// (UP-001) between a bundle's manifest, whose DriverConfig only names a
// keystore driver, and RenderAppINI, which needs the material itself.
func ResolveSecrets(ctx context.Context, driver keystore.Driver) (Secrets, error) {
	secretKey, err := driver.Resolve(ctx, KeySecretKey)
	if err != nil {
		return Secrets{}, fmt.Errorf("forge: resolve %s: %w", KeySecretKey, err)
	}
	internalToken, err := driver.Resolve(ctx, KeyInternalToken)
	if err != nil {
		return Secrets{}, fmt.Errorf("forge: resolve %s: %w", KeyInternalToken, err)
	}
	lfsJWTSecret, err := driver.Resolve(ctx, KeyLFSJWTSecret)
	if err != nil {
		return Secrets{}, fmt.Errorf("forge: resolve %s: %w", KeyLFSJWTSecret, err)
	}

	return Secrets{
		SecretKey:     secretKey.Reveal(),
		InternalToken: internalToken.Reveal(),
		LFSJWTSecret:  lfsJWTSecret.Reveal(),
	}, nil
}
