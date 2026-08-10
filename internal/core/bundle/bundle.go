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

// DirName is the bundle directory's name inside the project folder it
// serves (spec.md "The unit: one forge per project"). Sitting in the
// project rather than in a machine-wide config directory is what versions
// the forge definition with the code it serves and lets any teammate who
// can clone the project operate the instance.
const DirName = ".farrier"

// DirFor returns the default bundle directory for a project folder:
// DirName inside it. `init` uses this when the operator gives no explicit
// location (INIT-001), and the result is joined, not resolved — a relative
// project folder yields a relative bundle path, so a caller that wants an
// absolute one makes it absolute before calling.
func DirFor(project string) string {
	return filepath.Join(project, DirName)
}

// Exists reports whether dir already holds a bundle — the manifest, the
// rendered Compose directory, or both. Those are exactly the paths Save
// writes, so they are exactly what a second write would destroy.
//
// This is the check `init` makes before it creates anything (INIT-004).
// The bundle directory carries an instance's identity: the manifest names
// the domain and pins the images the running forge was deployed from, and
// spec.md ("Key material") holds that once `init` writes a piece of key
// material nothing may silently overwrite it. A second `init` over an
// initialized project would mint a fresh identity and leave the live
// instance running on one that no longer matches its definition.
//
// A torn bundle counts as existing. compose/ with no manifest — or the
// reverse — is what a crashed or interrupted init leaves behind, and
// completing it with newly generated key material is the outcome INIT-004
// exists to prevent. Recovering from that is the operator's call: remove
// the folder, or point init somewhere else.
//
// Nothing else in the directory matters. An empty .farrier/, or a folder
// of unrelated files an operator pointed init at, is not a bundle, and
// Save adds to such a directory rather than replacing what is in it.
func Exists(dir string) (bool, error) {
	for _, name := range []string{ManifestFile, ComposeDir} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("bundle: check %s: %w", path, err)
		}
	}
	return false, nil
}

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
//
// Compose files are staged in a temporary directory and swapped in with a
// single rename, so a re-save (e.g. upgrade bumping pinned digests) never
// leaves compose/ half-written. The swap itself — removing the old compose/
// and renaming the staged one into place — is two syscalls and is the only
// part that isn't atomic; a crash between them would leave compose/ missing
// until the next Save. Full multi-file transactional atomicity (also
// covering the manifest write) is out of scope at CORE-001 config-write
// scope and can be revisited if upgrade needs a stronger guarantee.
func (b *Bundle) Save(dir string) error {
	if err := b.Manifest.Validate(); err != nil {
		return err
	}
	if len(b.Compose) == 0 {
		return fmt.Errorf("bundle: at least one rendered Compose file is required")
	}

	raw, err := yaml.Marshal(&b.Manifest)
	if err != nil {
		return fmt.Errorf("bundle: encode manifest: %w", err)
	}

	composeDir := filepath.Join(dir, ComposeDir)
	stagingDir := composeDir + ".tmp"
	if err := os.RemoveAll(stagingDir); err != nil {
		return fmt.Errorf("bundle: clear %s: %w", stagingDir, err)
	}
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return fmt.Errorf("bundle: create %s: %w", stagingDir, err)
	}

	names := make([]string, 0, len(b.Compose))
	for name := range b.Compose {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := writeFile(filepath.Join(stagingDir, name), b.Compose[name]); err != nil {
			return err
		}
	}

	if err := os.RemoveAll(composeDir); err != nil {
		return fmt.Errorf("bundle: clear %s: %w", ComposeDir, err)
	}
	if err := os.Rename(stagingDir, composeDir); err != nil {
		return fmt.Errorf("bundle: install %s: %w", ComposeDir, err)
	}

	if err := writeFile(filepath.Join(dir, ManifestFile), raw); err != nil {
		return err
	}
	return nil
}

// writeFile writes content to a temp file next to path and renames it into
// place, so a reader never observes a partially written file.
func writeFile(path string, content []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return fmt.Errorf("bundle: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("bundle: write %s: %w", path, err)
	}
	return nil
}
