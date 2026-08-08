package keystore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileDriver resolves key material by reading files from a local
// directory (KEY-001): keyName is joined onto Path as a filename, so each
// piece of key material is its own file on disk, named for what it holds
// (e.g. Path/forgejo_secret_key). Config: {"path": "<directory>"}.
type FileDriver struct {
	Path string
}

// Resolve reads Path/keyName and returns its contents verbatim — no
// parsing, no trimming, so binary key material (certificates, host keys)
// round-trips exactly.
func (d FileDriver) Resolve(ctx context.Context, keyName string) (Secret, error) {
	if err := ctx.Err(); err != nil {
		return Secret{}, err
	}
	full, err := d.resolvePath(keyName)
	if err != nil {
		return Secret{}, err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return Secret{}, fmt.Errorf("keystore: file: key %q not found at %s: %w", keyName, full, ErrNotFound)
		}
		return Secret{}, fmt.Errorf("keystore: file: resolve key %q: %w", keyName, err)
	}
	if len(data) == 0 {
		return Secret{}, fmt.Errorf("keystore: file: key %q is empty at %s", keyName, full)
	}
	return NewSecret(string(data)), nil
}

// Store writes secret to Path/keyName, creating Path if it doesn't exist
// yet, overwriting whatever was there. It has no overwrite guard of its
// own: New wraps every Writer-implementing driver, this one included, in
// the rotation-policy guard (guardedDriver) that refuses to overwrite key
// material not declared rotating (spec.md "Identity" > "Key material") —
// enforced once, above the driver, rather than inside each one.
func (d FileDriver) Store(ctx context.Context, keyName string, secret Secret) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, err := d.resolvePath(keyName)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return fmt.Errorf("keystore: file: create directory for key %q: %w", keyName, err)
	}
	if err := os.WriteFile(full, []byte(secret.Reveal()), 0o600); err != nil {
		return fmt.Errorf("keystore: file: store key %q: %w", keyName, err)
	}
	return nil
}

// resolvePath validates keyName and joins it onto d.Path, rejecting any
// name that would escape the configured directory.
func (d FileDriver) resolvePath(keyName string) (string, error) {
	if strings.TrimSpace(keyName) == "" {
		return "", fmt.Errorf("keystore: file: key name is required")
	}
	base := filepath.Clean(d.Path)
	full := filepath.Join(base, keyName)
	if full != base && !strings.HasPrefix(full, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("keystore: file: key name %q escapes configured path", keyName)
	}
	return full, nil
}

func newFileDriver(config map[string]any) (Driver, error) {
	path, err := stringConfig(config, "path")
	if err != nil {
		return nil, fmt.Errorf("keystore: file: %w", err)
	}
	return FileDriver{Path: path}, nil
}
