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
func (d FileDriver) Resolve(ctx context.Context, keyName string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(keyName) == "" {
		return nil, fmt.Errorf("keystore: file: key name is required")
	}

	base := filepath.Clean(d.Path)
	full := filepath.Join(base, keyName)
	if full != base && !strings.HasPrefix(full, base+string(os.PathSeparator)) {
		return nil, fmt.Errorf("keystore: file: key name %q escapes configured path", keyName)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("keystore: file: resolve key %q: %w", keyName, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("keystore: file: key %q is empty at %s", keyName, full)
	}
	return data, nil
}

func newFileDriver(config map[string]any) (Driver, error) {
	path, err := stringConfig(config, "path")
	if err != nil {
		return nil, fmt.Errorf("keystore: file: %w", err)
	}
	return FileDriver{Path: path}, nil
}
