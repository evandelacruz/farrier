package bundle

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func validManifest() *Manifest {
	return NewManifest(
		"forge.example.com",
		map[string]string{
			"forgejo": "codeberg.org/forgejo/forgejo@sha256:" + fakeDigest,
		},
		DriverConfig{
			Keystore: DriverRef{Driver: "file", Config: map[string]any{"path": "/keys/bundle.key"}},
			Blob:     DriverRef{Driver: "local", Config: map[string]any{"path": "/data/blobs"}},
		},
		ACMEConfig{DNSProvider: "manual"},
	)
}

const fakeDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// testHostPublicKey is an OpenSSH authorized-keys line with a comment on
// it — the shape `init` stores and copies into the manifest.
const testHostPublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBundleTestHostKeyBlobAAAAAAAAAAAAAAAAAAAAA farrier@instance"

func validBundle() *Bundle {
	return &Bundle{
		Manifest: *validManifest(),
		Compose:  map[string][]byte{"docker-compose.yml": []byte("services: {}\n")},
	}
}

func TestManifestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr bool
	}{
		{"valid", func(m *Manifest) {}, false},
		// INIT-005: no domain is a nameless bundle, valid so long as the
		// ACME section it has no use for goes with it.
		{"nameless", func(m *Manifest) { m.Domain = ""; m.ACME = ACMEConfig{} }, false},
		{"nameless with an acme provider", func(m *Manifest) { m.Domain = "" }, true},
		{"nameless with an acme email", func(m *Manifest) { m.Domain = ""; m.ACME = ACMEConfig{Email: "ops@example.com"} }, true},
		{"nameless with an acme directory", func(m *Manifest) {
			m.Domain = ""
			m.ACME = ACMEConfig{DirectoryURL: "https://acme-staging-v02.api.letsencrypt.org/directory"}
		}, true},
		// The CA is optional on a named bundle: absent is a manifest
		// written before the bundle recorded which server issued its
		// certificate, and readers take Let's Encrypt production.
		{"named with no acme directory", func(m *Manifest) { m.ACME.DirectoryURL = "" }, false},
		{"named with an acme directory", func(m *Manifest) {
			m.ACME.DirectoryURL = "https://acme-staging-v02.api.letsencrypt.org/directory"
		}, false},
		{"no images", func(m *Manifest) { m.Images = nil }, true},
		{"tag-pinned image", func(m *Manifest) { m.Images["forgejo"] = "forgejo/forgejo:11" }, true},
		{"missing keystore driver", func(m *Manifest) { m.Drivers.Keystore.Driver = "" }, true},
		{"missing blob driver", func(m *Manifest) { m.Drivers.Blob.Driver = "" }, true},
		{"missing acme dns provider", func(m *Manifest) { m.ACME.DNSProvider = "" }, true},
		{"missing state kind", func(m *Manifest) { m.State = m.State[1:] }, true},
		{"duplicate state kind", func(m *Manifest) { m.State = append(m.State, m.State[0]) }, true},
		{"missing checksum algorithm", func(m *Manifest) { m.ChecksumAlgorithm = "" }, true},
		// The host public key is optional — absent is a manifest written
		// before the field existed — but a present one has to be readable
		// as a pin, since a pin nobody can parse is a push that fails at
		// the far end of the operation instead of here.
		{"no ssh host public key", func(m *Manifest) { m.SSHHostKeyPublic = "" }, false},
		{"valid ssh host public key", func(m *Manifest) { m.SSHHostKeyPublic = testHostPublicKey }, false},
		{"ssh host public key with no blob", func(m *Manifest) { m.SSHHostKeyPublic = "ssh-ed25519" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest()
			tc.mutate(m)
			err := m.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestNewManifestDeclaresAllFourStateKinds(t *testing.T) {
	m := validManifest()
	if len(m.State) != len(AllStateKinds) {
		t.Fatalf("got %d state declarations, want %d", len(m.State), len(AllStateKinds))
	}
	for _, kind := range AllStateKinds {
		found := false
		for _, d := range m.State {
			if d.Kind == kind {
				found = true
			}
		}
		if !found {
			t.Fatalf("state kind %q not declared", kind)
		}
	}
}

func TestSaveRejectsInvalidManifest(t *testing.T) {
	b := validBundle()
	b.Manifest.Images = nil
	if err := b.Save(t.TempDir()); err == nil {
		t.Fatal("Save() = nil, want error for invalid manifest")
	}
}

func TestSaveRejectsNoComposeFiles(t *testing.T) {
	b := validBundle()
	b.Compose = nil
	if err := b.Save(t.TempDir()); err == nil {
		t.Fatal("Save() = nil, want error for no Compose files")
	}
}

func TestLoadRejectsMissingCompose(t *testing.T) {
	dir := t.TempDir()
	raw, err := yaml.Marshal(&validBundle().Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load() = nil, want error for missing compose/ directory")
	}
}

func TestLoadRejectsEmptyComposeDir(t *testing.T) {
	dir := t.TempDir()
	raw, err := yaml.Marshal(&validBundle().Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ComposeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load() = nil, want error for empty compose/ directory")
	}
}

// TestSaveRemovesStaleComposeFiles proves a re-save doesn't leave behind
// compose files from a previous save that are no longer in b.Compose — the
// case that matters for upgrade, which re-renders compose files on every
// pinned-digest bump.
func TestSaveRemovesStaleComposeFiles(t *testing.T) {
	dir := t.TempDir()
	b := validBundle()
	b.Compose["old.yml"] = []byte("services:\n  old: {}\n")
	if err := b.Save(dir); err != nil {
		t.Fatalf("Save() = %v", err)
	}

	b.Compose = map[string][]byte{"docker-compose.yml": []byte("services: {}\n")}
	if err := b.Save(dir); err != nil {
		t.Fatalf("Save() = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ComposeDir, "old.yml")); !os.IsNotExist(err) {
		t.Fatalf("old.yml still present after re-save, err = %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if !reflect.DeepEqual(loaded.Compose, b.Compose) {
		t.Fatalf("loaded compose = %+v, want %+v", loaded.Compose, b.Compose)
	}
}

// TestSaveLeavesNoStagingDir proves the compose/ staging directory used to
// make the swap-in atomic doesn't leak into the bundle directory.
func TestSaveLeavesNoStagingDir(t *testing.T) {
	dir := t.TempDir()
	if err := validBundle().Save(dir); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ComposeDir+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("staging dir still present after Save, err = %v", err)
	}
}

// TestPortability proves the CORE-001 requirement directly: a bundle saved
// to one directory, then physically copied byte-for-byte to a wholly
// different directory (standing in for "another machine"), loads back to an
// identical in-memory bundle. Nothing about Bundle or Manifest depends on
// the directory it was first written to.
func TestPortability(t *testing.T) {
	original := validBundle()
	original.Compose["runner.yml"] = []byte("services:\n  runner: {}\n")

	srcDir := filepath.Join(t.TempDir(), "machine-a", "bundle")
	if err := original.Save(srcDir); err != nil {
		t.Fatalf("Save() = %v", err)
	}

	dstDir := filepath.Join(t.TempDir(), "machine-b", "elsewhere", "bundle")
	if err := copyDir(t, srcDir, dstDir); err != nil {
		t.Fatalf("copyDir() = %v", err)
	}

	loaded, err := Load(dstDir)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if !reflect.DeepEqual(loaded.Manifest, original.Manifest) {
		t.Fatalf("manifest changed after copy:\ngot  %+v\nwant %+v", loaded.Manifest, original.Manifest)
	}
	if !reflect.DeepEqual(loaded.Compose, original.Compose) {
		t.Fatalf("compose files changed after copy:\ngot  %+v\nwant %+v", loaded.Compose, original.Compose)
	}
}

func copyDir(t *testing.T, src, dst string) error {
	t.Helper()
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
}

// FORGE-005: an unset colocated-runner preference means enabled, so a
// manifest written before the field existed still gets the runner the
// requirement asks for; false is the operator's deliberate opt-out.
func TestColocatedRunnerDefaultsToEnabled(t *testing.T) {
	m := validManifest()
	if m.ColocatedRunnerDeclared() {
		t.Error("NewManifest wrote a colocated-runner preference; that is init's call")
	}
	if !m.ColocatedRunnerEnabled() {
		t.Error("an unset colocated-runner preference reads as disabled, want enabled")
	}

	for _, want := range []bool{true, false} {
		value := want
		m.Actions.ColocatedRunner = &value
		if !m.ColocatedRunnerDeclared() {
			t.Errorf("ColocatedRunner=%v does not read as declared", want)
		}
		if got := m.ColocatedRunnerEnabled(); got != want {
			t.Errorf("ColocatedRunnerEnabled() = %v, want %v", got, want)
		}
	}
}

func TestColocatedRunnerSurvivesSaveAndLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	b := validBundle()
	disabled := false
	b.Manifest.Actions.ColocatedRunner = &disabled

	if err := b.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !loaded.Manifest.ColocatedRunnerDeclared() {
		t.Fatal("the colocated-runner preference did not round-trip")
	}
	if loaded.Manifest.ColocatedRunnerEnabled() {
		t.Error("a bundle saved with the colocated runner off loaded with it on")
	}
}

// FORGE-005: the job image is a manifest field with the lean default the
// constant used to hold. A manifest that names none — one written before the
// field existed — resolves to it rather than to nothing.
func TestActionsJobImageDefaultsToTheLeanImage(t *testing.T) {
	m := validManifest()
	if m.Actions.JobImage != "" {
		t.Error("NewManifest wrote a job image; that is init's call")
	}
	if got := m.ActionsJobImageOrDefault(); got != DefaultActionsJobImage {
		t.Errorf("ActionsJobImageOrDefault() = %q, want %q", got, DefaultActionsJobImage)
	}

	const chosen = "ghcr.io/catthehacker/ubuntu:act-22.04"
	m.Actions.JobImage = chosen
	if got := m.ActionsJobImageOrDefault(); got != chosen {
		t.Errorf("ActionsJobImageOrDefault() = %q, want the manifest's %q", got, chosen)
	}

	m.Actions.JobImage = "   "
	if got := m.ActionsJobImageOrDefault(); got != DefaultActionsJobImage {
		t.Errorf("a blank job image resolved to %q, want the default", got)
	}
}

// Unlike Images, the job image is not digest-pinned: it is what future jobs
// run in rather than what this instance is, so a plain tag is valid and an
// operator can move it without re-running init.
func TestValidateAcceptsAnUnpinnedJobImage(t *testing.T) {
	m := validManifest()
	m.Actions.JobImage = "ghcr.io/catthehacker/ubuntu:act-22.04"
	if err := m.Validate(); err != nil {
		t.Errorf("Validate rejected an unpinned job image: %v", err)
	}
}

// It is written into the runner's configuration file once per label, so a
// value carrying whitespace is either a broken reference or configuration
// smuggled in through the manifest.
func TestValidateRejectsAJobImageWithWhitespace(t *testing.T) {
	for _, image := range []string{
		"node:22 --privileged",
		"node:22\nprivileged: true",
		"node:22\ttrailing",
	} {
		m := validManifest()
		m.Actions.JobImage = image
		if err := m.Validate(); err == nil {
			t.Errorf("Validate accepted job image %q", image)
		}
	}
}

func TestActionsJobImageSurvivesSaveAndLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	b := validBundle()
	const chosen = "ghcr.io/catthehacker/ubuntu:act-22.04"
	b.Manifest.Actions.JobImage = chosen

	if err := b.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := loaded.Manifest.ActionsJobImageOrDefault(); got != chosen {
		t.Errorf("job image loaded as %q, want %q", got, chosen)
	}
}

// INIT-005: a nameless bundle survives Save and Load like any other, and the
// manifest it writes omits the domain key rather than writing an empty one.
func TestNamelessBundleSurvivesSaveAndLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	b := validBundle()
	b.Manifest.Domain = ""
	b.Manifest.ACME = ACMEConfig{}

	if err := b.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Manifest.Named() {
		t.Errorf("loaded domain = %q, want a nameless bundle", loaded.Manifest.Domain)
	}

	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var keys map[string]any
	if err := yaml.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if _, ok := keys["domain"]; ok {
		t.Errorf("manifest carries a domain key:\n%s", raw)
	}
	if _, ok := keys["acme"]; ok {
		t.Errorf("nameless manifest carries an acme section:\n%s", raw)
	}
}

func TestManifestNamed(t *testing.T) {
	m := validManifest()
	if !m.Named() {
		t.Error("Named() = false for a manifest with a domain")
	}
	m.Domain = "  "
	if m.Named() {
		t.Error("Named() = true for a whitespace-only domain")
	}
}

// TestGitSSHPortDefaultsTo2222 pins UP-005's default: a manifest that names
// no port serves git over SSH on 2222, and NewManifest leaves the field
// unset rather than writing the default in — what a bundle publishes is
// init's call to record, not the constructor's.
func TestGitSSHPortDefaultsTo2222(t *testing.T) {
	m := validManifest()
	if m.GitSSHPort != 0 {
		t.Errorf("NewManifest wrote GitSSHPort = %d; that is init's call", m.GitSSHPort)
	}
	if got := m.GitSSHPortOrDefault(); got != DefaultGitSSHPort {
		t.Errorf("GitSSHPortOrDefault() = %d, want %d", got, DefaultGitSSHPort)
	}
	if DefaultGitSSHPort != 2222 {
		t.Errorf("DefaultGitSSHPort = %d, want 2222 (spec.md \"Reaching the forge\")", DefaultGitSSHPort)
	}

	m.GitSSHPort = 22
	if got := m.GitSSHPortOrDefault(); got != 22 {
		t.Errorf("GitSSHPortOrDefault() = %d, want the manifest's own 22", got)
	}
}

// TestGitSSHPortValidation covers the range a manifest may declare: zero is
// unset, real ports pass, anything unpublishable is refused before a
// deployment can try to bind it.
func TestGitSSHPortValidation(t *testing.T) {
	cases := []struct {
		port    int
		wantErr bool
	}{
		{0, false},
		{22, false},
		{2222, false},
		{65535, false},
		{-1, true},
		{65536, true},
	}
	for _, tc := range cases {
		if err := ValidateGitSSHPort(tc.port); (err != nil) != tc.wantErr {
			t.Errorf("ValidateGitSSHPort(%d) = %v, wantErr %v", tc.port, err, tc.wantErr)
		}
		m := validManifest()
		m.GitSSHPort = tc.port
		if err := m.Validate(); (err != nil) != tc.wantErr {
			t.Errorf("Validate() with GitSSHPort=%d = %v, wantErr %v", tc.port, err, tc.wantErr)
		}
	}
}

// TestGitSSHPortSurvivesSaveAndLoad is the bundle-identity half of UP-005:
// the port is carried by the bundle, so any machine holding the bundle
// deploys the same endpoint, and a restored instance answers where the
// original did (RSTR-004, XCUT-001).
func TestGitSSHPortSurvivesSaveAndLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	b := validBundle()
	b.Manifest.GitSSHPort = 22

	if err := b.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := loaded.Manifest.GitSSHPortOrDefault(); got != 22 {
		t.Errorf("loaded git-over-ssh port = %d, want the saved 22", got)
	}
}

// The host public key travels with the bundle, under a manifest key an
// operator can read: it is what lets someone publish to a shared instance
// pin its identity without holding the keystore (CORE-001, IMPT-004).
func TestSSHHostPublicKeySurvivesSaveAndLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	b := validBundle()
	b.Manifest.SSHHostKeyPublic = testHostPublicKey

	if err := b.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var fields map[string]any
	if err := yaml.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if got := fields["sshHostKeyPublic"]; got != testHostPublicKey {
		t.Errorf("sshHostKeyPublic = %v, want %q", got, testHostPublicKey)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Manifest.SSHHostKeyPublic != testHostPublicKey {
		t.Errorf("loaded ssh host public key = %q, want %q", loaded.Manifest.SSHHostKeyPublic, testHostPublicKey)
	}
}

// A manifest that carries no host public key is one written before the
// field existed, and it must not start emitting an empty key.
func TestSSHHostPublicKeyIsOmittedWhenUnset(t *testing.T) {
	raw, err := yaml.Marshal(validManifest())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "sshHostKeyPublic") {
		t.Errorf("manifest = %s, want no sshHostKeyPublic key when none is set", raw)
	}
}

func TestSSHKnownHostsLineFor(t *testing.T) {
	cases := []struct {
		name string
		port int
		want string
	}{
		// The comment is dropped in both: OpenSSH would read whatever
		// follows the blob as a further host-key option.
		{"default port is bracketed", 0, "[forge.example.com]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBundleTestHostKeyBlobAAAAAAAAAAAAAAAAAAAAA\n"},
		{"port 22 is a bare hostname", 22, "forge.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBundleTestHostKeyBlobAAAAAAAAAAAAAAAAAAAAA\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest()
			m.GitSSHPort = tc.port
			got, err := m.SSHKnownHostsLineFor(testHostPublicKey)
			if err != nil {
				t.Fatalf("SSHKnownHostsLineFor: %v", err)
			}
			if got != tc.want {
				t.Errorf("line = %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := validManifest().SSHKnownHostsLineFor("ssh-ed25519"); err == nil {
		t.Error("SSHKnownHostsLineFor with no blob = nil error, want a refusal")
	}
}

// UP-006: a nameless instance is pinned at the address it is reached at,
// through the same renderer, so the entry and the clone URL name one host.
func TestSSHKnownHostsLineAt(t *testing.T) {
	cases := []struct {
		name string
		port int
		host string
		want string
	}{
		{"an address on the default port", 0, "192.168.1.5", "[192.168.1.5]:2222"},
		{"an address on port 22", 22, "192.168.1.5", "192.168.1.5"},
		// A URL authority brackets an IPv6 literal; a known_hosts entry's
		// brackets belong to the port, so they are not nested.
		{"an IPv6 literal on the default port", 0, "[fd00::1]", "[fd00::1]:2222"},
		{"an IPv6 literal on port 22", 22, "[fd00::1]", "fd00::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manifest{GitSSHPort: tc.port}
			if got := m.GitSSHKnownHostsHostAt(tc.host); got != tc.want {
				t.Errorf("GitSSHKnownHostsHostAt(%q) = %q, want %q", tc.host, got, tc.want)
			}
			line, err := m.SSHKnownHostsLineAt(tc.host, testHostPublicKey)
			if err != nil {
				t.Fatalf("SSHKnownHostsLineAt: %v", err)
			}
			if !strings.HasPrefix(line, tc.want+" ssh-ed25519 ") {
				t.Errorf("line = %q, want it keyed at %q", line, tc.want)
			}
		})
	}
}

// INIT-004: Exists is what stands between a re-run of `init` and a live
// instance's identity, so it has to answer for every shape a bundle
// directory turns up in — including the torn ones.
func TestExists(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  bool
	}{
		{
			name:  "absent directory",
			setup: func(t *testing.T, dir string) {},
			want:  false,
		},
		{
			name: "empty directory",
			setup: func(t *testing.T, dir string) {
				mkdir(t, dir)
			},
			want: false,
		},
		{
			name: "unrelated files only",
			setup: func(t *testing.T, dir string) {
				mkdir(t, dir)
				writeTestFile(t, filepath.Join(dir, "README.md"), "not a bundle")
			},
			want: false,
		},
		{
			name: "saved bundle",
			setup: func(t *testing.T, dir string) {
				if err := validBundle().Save(dir); err != nil {
					t.Fatalf("Save: %v", err)
				}
			},
			want: true,
		},
		{
			name: "manifest without compose",
			setup: func(t *testing.T, dir string) {
				mkdir(t, dir)
				writeTestFile(t, filepath.Join(dir, ManifestFile), "domain: forge.example.com\n")
			},
			want: true,
		},
		{
			name: "compose without manifest",
			setup: func(t *testing.T, dir string) {
				mkdir(t, filepath.Join(dir, ComposeDir))
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), DirName)
			tc.setup(t, dir)

			got, err := Exists(dir)
			if err != nil {
				t.Fatalf("Exists: %v", err)
			}
			if got != tc.want {
				t.Errorf("Exists(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestGitSSHCloneURLAt is UP-006's half of the same spelling: a nameless
// bundle has no domain, so `up` reports the clone URL at the address the
// operator supplied — through the same function, with the same port and
// the same rules.
func TestGitSSHCloneURLAt(t *testing.T) {
	m := validManifest()
	m.Domain = ""

	if got, want := m.GitSSHCloneURLAt("box.local", "acme", "widgets"), "ssh://git@box.local:2222/acme/widgets.git"; got != want {
		t.Errorf("GitSSHCloneURLAt() = %q, want %q", got, want)
	}

	m.GitSSHPort = 22
	if got, want := m.GitSSHCloneURLAt("192.168.1.5", "acme", "widgets"), "git@192.168.1.5:acme/widgets.git"; got != want {
		t.Errorf("GitSSHCloneURLAt() on 22 = %q, want %q", got, want)
	}
	// A bracketed IPv6 literal always takes the ssh:// form: scp-style is
	// ambiguous once the host itself carries colons.
	if got, want := m.GitSSHCloneURLAt("[fd00::1]", "acme", "widgets"), "ssh://git@[fd00::1]:22/acme/widgets.git"; got != want {
		t.Errorf("GitSSHCloneURLAt() on an IPv6 literal = %q, want %q", got, want)
	}
}

// TestGitSSHCloneURL pins the spelling every caller shares: the URL `up`
// reports and the URL `publish` writes into a project's origin (IMPT-004)
// come from this one function, so they cannot drift apart. Port 22 renders
// scp-style and anything else carries the port, matching what Forgejo
// itself displays (spec.md "Reaching the forge").
func TestGitSSHCloneURL(t *testing.T) {
	m := validManifest()
	m.Domain = "git.example.com"

	if got, want := m.GitSSHCloneURL("acme", "widgets"), "ssh://git@git.example.com:2222/acme/widgets.git"; got != want {
		t.Errorf("GitSSHCloneURL() = %q, want %q", got, want)
	}
	if got, want := m.GitSSHKnownHostsHost(), "[git.example.com]:2222"; got != want {
		t.Errorf("GitSSHKnownHostsHost() = %q, want %q", got, want)
	}

	m.GitSSHPort = 22
	if got, want := m.GitSSHCloneURL("acme", "widgets"), "git@git.example.com:acme/widgets.git"; got != want {
		t.Errorf("GitSSHCloneURL() on 22 = %q, want %q", got, want)
	}
	if got, want := m.GitSSHKnownHostsHost(), "git.example.com"; got != want {
		t.Errorf("GitSSHKnownHostsHost() on 22 = %q, want %q", got, want)
	}
}

// The ACME server a bundle's certificates are issued against survives Save
// and Load under a stable manifest key, because renewal reads it back on
// every later `up` — a value that did not round-trip would send a bundle
// rehearsed against staging to production the next time its certificate
// came due.
func TestACMEDirectoryURLSurvivesSaveAndLoad(t *testing.T) {
	const staging = "https://acme-staging-v02.api.letsencrypt.org/directory"
	dir := filepath.Join(t.TempDir(), "bundle")
	b := validBundle()
	b.Manifest.ACME.DirectoryURL = staging

	if err := b.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Manifest.ACME.DirectoryURL != staging {
		t.Errorf("loaded acme directory = %q, want %q", loaded.Manifest.ACME.DirectoryURL, staging)
	}

	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var parsed struct {
		ACME map[string]any `yaml:"acme"`
	}
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if got := parsed.ACME["directoryUrl"]; got != staging {
		t.Errorf("manifest acme.directoryUrl = %v, want %q:\n%s", got, staging, raw)
	}
}

// A bundle written before the manifest recorded a CA still loads, still
// validates, and still reports no CA — this repository has live bundles on
// disk, and adding a field must not turn one of them into a bundle that no
// longer loads.
func TestManifestWithoutACMEDirectoryURLStillLoads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	b := validBundle()
	b.Manifest.ACME = ACMEConfig{DNSProvider: "cloudflare", Email: "ops@example.com"}
	if err := b.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if strings.Contains(string(raw), "directoryUrl") {
		t.Errorf("manifest carries an empty directoryUrl key:\n%s", raw)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := loaded.Manifest.Validate(); err != nil {
		t.Errorf("Validate on a manifest with no acme directory: %v", err)
	}
	if loaded.Manifest.ACME.DirectoryURL != "" {
		t.Errorf("acme directory = %q, want it empty", loaded.Manifest.ACME.DirectoryURL)
	}
}
