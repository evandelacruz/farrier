package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// installKeys installs every key-material component the snapshot at
// plainDir captured into target, mirroring initialize.Run's write path
// (INIT-003) — the recovery this exists for is the target keystore having
// lost what init originally wrote there, not just the forge host, so a
// snapshot's captured key material is what STATE-004 keeps that possible
// from.
//
// The common case is the opposite: the operator still holds the same
// keystore target the bundle has always used, already populated. Unlike
// initialize.Run, which only ever writes into an empty keystore,
// installKeys must be safe to run against one that already holds this
// bundle's key material unchanged (UP-003's re-run safety, applied here):
// for each name, it resolves the existing value first — nothing to do if
// it already matches what the snapshot captured, a loud refusal if it
// resolves to something different (the keystore's guardedDriver.Store
// would refuse the overwrite anyway, but comparing first tells the
// operator whether the mismatch is even worth investigating), and only
// calls Store when the target genuinely has nothing there yet.
func installKeys(ctx context.Context, job *events.Job, plainDir string, manifest *backup.Manifest, target keystore.Driver) error {
	job.Started(StepInstallKeys, "installing key material")

	writer, ok := target.(keystore.Writer)
	if !ok {
		err := errors.New("restore: install key material: target keystore driver cannot store key material")
		job.Emit(StepInstallKeys, events.StateFailed, err.Error())
		return err
	}

	components := make(map[string]backup.Component, len(manifest.Components))
	for _, c := range manifest.Components {
		if c.Kind == bundle.StateKindKeys {
			components[c.Name] = c
		}
	}

	installed := 0
	for _, name := range keyNames() {
		if err := ctx.Err(); err != nil {
			job.Emit(StepInstallKeys, events.StateFailed, err.Error())
			return err
		}
		c, ok := components[name]
		if !ok {
			// backup.Verify's completeness check already refuses a
			// snapshot missing any of these before decryptAndVerify
			// returns, so this only guards against a caller skipping that
			// step.
			err := fmt.Errorf("restore: install key material: %s: not present in snapshot", name)
			job.Emit(StepInstallKeys, events.StateFailed, err.Error())
			return err
		}

		did, err := installOneKey(ctx, plainDir, c, name, target, writer)
		if err != nil {
			job.Emit(StepInstallKeys, events.StateFailed, err.Error())
			return err
		}
		if did {
			installed++
		}
	}

	job.Emit(StepInstallKeys, events.StateSucceeded, fmt.Sprintf("installed %d key(s), %d already present", installed, len(keyNames())-installed))
	return nil
}

// installOneKey installs one key's captured value from plainDir into
// target under name, unless target already resolves name to the identical
// value. It reports whether it actually wrote anything.
func installOneKey(ctx context.Context, plainDir string, c backup.Component, name string, target keystore.Driver, writer keystore.Writer) (bool, error) {
	data, err := os.ReadFile(filepath.Join(plainDir, filepath.FromSlash(c.Path)))
	if err != nil {
		return false, fmt.Errorf("restore: install key material: read %s: %w", name, err)
	}
	secret := keystore.NewSecret(string(data))

	existing, err := target.Resolve(ctx, name)
	switch {
	case err == nil:
		if existing.Reveal() == secret.Reveal() {
			return false, nil
		}
		return false, fmt.Errorf("restore: install key material: %s: target keystore already holds different key material under this name", name)
	case errors.Is(err, keystore.ErrNotFound):
		if err := writer.Store(ctx, name, secret); err != nil {
			return false, fmt.Errorf("restore: install key material: store %s: %w", name, err)
		}
		return true, nil
	default:
		return false, fmt.Errorf("restore: install key material: resolve %s: %w", name, err)
	}
}
