package keystore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileResolver is the "file" keystore driver (KEY-001): it resolves key
// material from a local path, keyName joined onto Dir. A trailing newline
// on the file's content is trimmed, since key material is typically
// written by an editor or `echo` that appends one.
type FileResolver struct {
	Dir string
}

// NewFile returns a FileResolver rooted at dir.
func NewFile(dir string) (*FileResolver, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("keystore: file: dir is required")
	}
	return &FileResolver{Dir: dir}, nil
}

// Resolve reads keyName as a file under Dir.
func (r *FileResolver) Resolve(ctx context.Context, keyName string) (Secret, error) {
	if err := ctx.Err(); err != nil {
		return Secret{}, err
	}
	path, err := r.resolve(keyName)
	if err != nil {
		return Secret{}, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Secret{}, fmt.Errorf("keystore: file: key %q not found", keyName)
		}
		return Secret{}, fmt.Errorf("keystore: file: read key %q: %w", keyName, err)
	}
	return NewSecret(strings.TrimRight(string(content), "\r\n")), nil
}

// resolve turns keyName into a filesystem path under Dir, rejecting any
// name that would escape it (an absolute path, or one containing a ".."
// segment) — the same guard blob.LocalAdapter applies to object keys.
func (r *FileResolver) resolve(keyName string) (string, error) {
	if strings.TrimSpace(keyName) == "" {
		return "", errors.New("keystore: file: key name is required")
	}
	clean := filepath.Clean(filepath.FromSlash(keyName))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("keystore: file: key %q escapes dir", keyName)
	}
	return filepath.Join(r.Dir, clean), nil
}
