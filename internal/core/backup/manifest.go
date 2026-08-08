package backup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/evandelacruz/farrier/internal/core/bundle"
)

// ManifestFile is the snapshot manifest's filename within a snapshot
// directory (tech-spec.md "Snapshot format").
const ManifestFile = "snapshot-manifest.json"

// Component is one captured file's entry in the snapshot manifest: which of
// the four state kinds it belongs to, the name that identifies it within
// that kind (a "<owner>/<repo>" remote name, a blob key, a key material
// name, or the database's own file name), its path relative to the snapshot
// directory, and its checksum.
type Component struct {
	Kind     bundle.StateKind `json:"kind"`
	Name     string           `json:"name"`
	Path     string           `json:"path"`
	Checksum string           `json:"checksum"`
}

// Manifest is the snapshot's own manifest — snapshot-manifest.json
// (tech-spec.md "Snapshot format") — distinct from the bundle manifest
// (bundle.Manifest, farrier.yaml): the exact Forgejo version the snapshot
// was captured from (spec.md "Version pinning"), when capture ran, the
// checksum algorithm every Component uses, and one Component per captured
// file across all four state kinds (BKUP-001).
type Manifest struct {
	ForgejoVersion    string      `json:"forgejoVersion"`
	Timestamp         time.Time   `json:"timestamp"`
	ChecksumAlgorithm string      `json:"checksumAlgorithm"`
	Components        []Component `json:"components"`
}

// writeManifest writes manifest as ManifestFile inside dir.
func writeManifest(dir string, manifest *Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("backup: marshal manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), data, 0o600); err != nil {
		return fmt.Errorf("backup: write manifest: %w", err)
	}
	return nil
}
