package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LocalAdapter is the "local" blob adapter (BLOB-001): List, Get, and Put
// against a filesystem path. Object keys are slash-separated paths relative
// to Root; Put stages writes in a temp file and renames into place so a
// concurrent Get never observes a partially written object.
type LocalAdapter struct {
	Root string
}

// NewLocal returns a LocalAdapter rooted at root, creating root if it does
// not already exist. root must be an absolute path: a relative one would
// resolve against whatever directory the current process happens to be
// running from, which differs across machines and invocations — exactly
// the coupling to "the machine that ran init" XCUT-001 forbids.
func NewLocal(root string) (*LocalAdapter, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("blob: local: root is required")
	}
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("blob: local: root must be an absolute path, got %q", root)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("blob: local: create root %s: %w", root, err)
	}
	return &LocalAdapter{Root: root}, nil
}

// List returns every object under Root whose slash-separated key has the
// given prefix.
func (a *LocalAdapter) List(ctx context.Context, prefix string) ([]Object, error) {
	var objects []Object
	err := filepath.WalkDir(a.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(a.Root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		objects = append(objects, Object{Key: key, Size: info.Size(), Modified: info.ModTime()})
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return objects, nil
		}
		return nil, fmt.Errorf("blob: local: list: %w", err)
	}
	return objects, nil
}

// Get opens a stream for the object at key.
func (a *LocalAdapter) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	path, err := a.resolve(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("blob: local: get %s: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("blob: local: get %s: %w", key, err)
	}
	return f, nil
}

// Put streams r to the object at key, creating any parent directories
// needed, then swaps it into place with a single rename so readers never
// see a partial write.
func (a *LocalAdapter) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	path, err := a.resolve(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("blob: local: put %s: %w", key, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".blob-*.tmp")
	if err != nil {
		return fmt.Errorf("blob: local: put %s: %w", key, err)
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("blob: local: put %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("blob: local: put %s: %w", key, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("blob: local: put %s: %w", key, err)
	}
	return nil
}

// resolve turns a logical key into a filesystem path under Root, rejecting
// any key that would escape it (an absolute path, or one containing a ".."
// segment).
func (a *LocalAdapter) resolve(key string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", errors.New("blob: local: key is required")
	}
	clean := filepath.Clean(filepath.FromSlash(key))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("blob: local: key %q escapes root", key)
	}
	return filepath.Join(a.Root, clean), nil
}
