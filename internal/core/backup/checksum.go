package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// writeChecksummed copies r into a fresh file at destPath, creating any
// missing parent directories, and returns the hex-encoded sha256 checksum of
// what it wrote (bundle.DefaultChecksumAlgorithm — the algorithm every
// snapshot manifest declares). The caller still owns closing r.
func writeChecksummed(destPath string, r io.Reader) (string, error) {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return "", fmt.Errorf("create directory for %s: %w", destPath, err)
	}
	f, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", destPath, err)
	}

	h := sha256.New()
	_, copyErr := io.Copy(f, io.TeeReader(r, h))
	closeErr := f.Close()
	if copyErr != nil {
		return "", fmt.Errorf("write %s: %w", destPath, copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("write %s: %w", destPath, closeErr)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
