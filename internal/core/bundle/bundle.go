package bundle

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// ManifestFile is the manifest's filename within a bundle directory.
const ManifestFile = "farrier.yaml"

// ComposeDir is the directory within a bundle holding rendered Compose files.
const ComposeDir = "compose"

// Bundle is a bundle directory's contents in memory: the manifest and the
// rendered Compose files. It carries no reference to the directory it came
// from, so loading it from one path and saving it to another is exactly the
// "copied to another machine" case CORE-001 requires to work.
type Bundle struct {
	Manifest Manifest
	// Compose maps a filename under compose/ (e.g. "docker-compose.yml") to
	// its rendered contents.
	Compose map[string][]byte
}

// Load reads a bundle directory: the manifest at dir/farrier.yaml and every
// file under dir/compose/. It returns an error if the manifest fails
// Validate.
func Load(dir string) (*Bundle, error) {
	manifestPath := filepath.Join(dir, ManifestFile)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("bundle: read manifest: %w", err)
	}

	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("bundle: parse manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}

	compose, err := readComposeDir(filepath.Join(dir, ComposeDir))
	if err != nil {
		return nil, err
	}

	return &Bundle{Manifest: m, Compose: compose}, nil
}

func readComposeDir(dir string) (map[string][]byte, error) {
	compose := make(map[string][]byte)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("bundle: %s: no rendered Compose files", ComposeDir)
		}
		return nil, fmt.Errorf("bundle: read %s: %w", ComposeDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("bundle: read %s: %w", filepath.Join(ComposeDir, entry.Name()), err)
		}
		compose[entry.Name()] = content
	}
	if len(compose) == 0 {
		return nil, fmt.Errorf("bundle: %s: no rendered Compose files", ComposeDir)
	}
	return compose, nil
}

// Save writes the bundle to dir: the manifest to dir/farrier.yaml and each
// Compose file under dir/compose/. It validates the manifest first and
// refuses to write an incomplete bundle.
func (b *Bundle) Save(dir string) error {
	if err := b.Manifest.Validate(); err != nil {
		return err
	}
	if len(b.Compose) == 0 {
		return fmt.Errorf("bundle: at least one rendered Compose file is required")
	}

	composeDir := filepath.Join(dir, ComposeDir)
	if err := os.RemoveAll(composeDir); err != nil {
		return fmt.Errorf("bundle: clear %s: %w", ComposeDir, err)
	}
	if err := os.MkdirAll(composeDir, 0o755); err != nil {
		return fmt.Errorf("bundle: create %s: %w", ComposeDir, err)
	}

	raw, err := yaml.Marshal(&b.Manifest)
	if err != nil {
		return fmt.Errorf("bundle: encode manifest: %w", err)
	}
	if err := writeFile(filepath.Join(dir, ManifestFile), raw); err != nil {
		return err
	}

	names := make([]string, 0, len(b.Compose))
	for name := range b.Compose {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := writeFile(filepath.Join(composeDir, name), b.Compose[name]); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(path string, content []byte) error {
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("bundle: write %s: %w", path, err)
	}
	return nil
}
