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
		return Secret{}, fmt.Errorf("keystore: file: resolve key %q: %w", keyName, err)
	}
	if len(data) == 0 {
		return Secret{}, fmt.Errorf("keystore: file: key %q is empty at %s", keyName, full)
	}
	return NewSecret(string(data)), nil
}

// Store writes secret to Path/keyName, creating Path if it doesn't exist
// yet. It refuses to overwrite a key that already has content: key
// material is bundle identity (spec.md "Identity lives in the bundle, not
// the host"), so silently replacing it — the way a second `init` run
// against an existing keystore target could — would change what every
// existing clone, backup, and restore expects without anyone deciding to
// rotate it.
func (d FileDriver) Store(ctx context.Context, keyName string, secret Secret) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, err := d.resolvePath(keyName)
	if err != nil {
		return err
	}
	if data, err := os.ReadFile(full); err == nil && len(data) > 0 {
		return fmt.Errorf("keystore: file: key %q already exists at %s, refusing to overwrite", keyName, full)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("keystore: file: check existing key %q: %w", keyName, err)
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
