package orchestrate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
)

func TestComposeCommandIncludesProjectAndFiles(t *testing.T) {
	cmd, err := ComposeCommand("/opt/farrier", testBundle())
	if err != nil {
		t.Fatalf("ComposeCommand: %v", err)
	}
	if !strings.Contains(cmd, "cd '/opt/farrier'") {
		t.Errorf("command = %q, want cd into remote dir", cmd)
	}
	if !strings.Contains(cmd, "COMPOSE_PROJECT_NAME=") {
		t.Errorf("command = %q, want it to set COMPOSE_PROJECT_NAME", cmd)
	}
	if !strings.Contains(cmd, ProjectPath("/opt/farrier")) {
		t.Errorf("command = %q, want it to read the project name from %s", cmd, ProjectPath("/opt/farrier"))
	}
	if !strings.Contains(cmd, "compose/docker-compose.yml") {
		t.Errorf("command = %q, want it to reference the shipped file", cmd)
	}
}

func TestComposeCommandSortsFiles(t *testing.T) {
	b := &bundle.Bundle{
		Manifest: *testManifest(),
		Compose: map[string][]byte{
			"zzz.yml": []byte("z\n"),
			"aaa.yml": []byte("a\n"),
		},
	}
	cmd, err := ComposeCommand("/opt/farrier", b)
	if err != nil {
		t.Fatalf("ComposeCommand: %v", err)
	}
	aIdx := strings.Index(cmd, "aaa.yml")
	zIdx := strings.Index(cmd, "zzz.yml")
	if aIdx == -1 || zIdx == -1 || aIdx > zIdx {
		t.Errorf("command doesn't list files in sorted order: %q", cmd)
	}
}

func TestComposeCommandRejectsEmptyRemoteDir(t *testing.T) {
	if _, err := ComposeCommand("", testBundle()); err == nil {
		t.Fatal("ComposeCommand: want error for empty remote directory, got nil")
	}
}

func TestComposeCommandRejectsBundleWithNoCompose(t *testing.T) {
	b := &bundle.Bundle{Manifest: *testManifest()}
	if _, err := ComposeCommand("/opt/farrier", b); err == nil {
		t.Fatal("ComposeCommand: want error for bundle with no compose files, got nil")
	}
}

// resolveProject runs the shell ComposeCommand emits and reports the
// COMPOSE_PROJECT_NAME the host would actually see. The resolution happens
// in the shell, on the host, so a test that only inspected the string would
// be checking the wrong thing.
func resolveProject(t *testing.T, remoteDir string) string {
	t.Helper()
	cmd, err := ComposeCommand(remoteDir, testBundle())
	if err != nil {
		t.Fatalf("ComposeCommand: %v", err)
	}
	out, err := exec.Command("/bin/sh", "-c", cmd+" printenv COMPOSE_PROJECT_NAME").Output()
	if err != nil {
		t.Fatalf("run %q: %v", cmd, err)
	}
	return strings.TrimSpace(string(out))
}

// TestComposeCommandResolvesThePinnedProject is the whole point of the
// record: a command reaches the project the host says its containers are
// in, not one derived afresh by whoever is calling.
func TestComposeCommandResolvesThePinnedProject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(ProjectPath(dir), []byte("farrier-git-example-com-0badc0de\n"), 0o644); err != nil {
		t.Fatalf("write record: %v", err)
	}
	if got := resolveProject(t, dir); got != "farrier-git-example-com-0badc0de" {
		t.Errorf("resolved project = %q, want the pinned name", got)
	}
}

// TestComposeCommandFallsBackToTheLegacyProject is the migration path. A
// host deployed by a binary that predates the record has none, and its
// containers are in the project the old constant named — so that is what an
// absent record must resolve to, or `status`, `backup`, and `upgrade` stop
// seeing an instance that is running fine.
func TestComposeCommandFallsBackToTheLegacyProject(t *testing.T) {
	if got := resolveProject(t, t.TempDir()); got != LegacyProjectName {
		t.Errorf("resolved project = %q, want %q for a host with no record", got, LegacyProjectName)
	}
}

// A record that exists but is empty is a torn write, and an empty project
// name is not a project Compose can resolve. It takes the same fallback.
func TestComposeCommandFallsBackOnAnEmptyRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(ProjectPath(dir), nil, 0o644); err != nil {
		t.Fatalf("write record: %v", err)
	}
	if got := resolveProject(t, dir); got != LegacyProjectName {
		t.Errorf("resolved project = %q, want %q for an empty record", got, LegacyProjectName)
	}
}

// composeProjectName is the character set Compose accepts for a project
// name: lowercase alphanumerics, dashes and underscores, opening with a
// letter or a digit.
var composeProjectName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func TestProjectNameForDiffersBetweenBundles(t *testing.T) {
	a := &bundle.Bundle{Manifest: *testManifest()}
	other := *testManifest()
	other.Domain = "other.example.com"
	b := &bundle.Bundle{Manifest: other}

	if ProjectNameFor("/opt/farrier", a) == ProjectNameFor("/opt/farrier", b) {
		t.Errorf("two bundles share a project name: %q", ProjectNameFor("/opt/farrier", a))
	}
}

// TestProjectNameForDiffersBetweenRemoteDirs is the defect this record
// exists for: a drill restores the live instance's own snapshot from the
// live instance's own bundle, so the bundle cannot tell the two deployments
// apart and the remote directory has to.
func TestProjectNameForDiffersBetweenRemoteDirs(t *testing.T) {
	b := testBundle()
	if ProjectNameFor("/opt/farrier", b) == ProjectNameFor("/opt/farrier-drill", b) {
		t.Errorf("two deployments of one bundle share a project name: %q", ProjectNameFor("/opt/farrier", b))
	}
}

// The same deployment derives the same name every time it is asked, or a
// second command against a live instance addresses a project that does not
// exist and the instance is orphaned.
func TestProjectNameForIsStableAcrossCalls(t *testing.T) {
	b := testBundle()
	want := ProjectNameFor("/opt/farrier", b)
	for i := 0; i < 3; i++ {
		if got := ProjectNameFor("/opt/farrier", b); got != want {
			t.Fatalf("call %d derived %q, want %q", i, got, want)
		}
	}
	// A path spelled with a trailing slash or a redundant element is the
	// same directory, and must not be a different deployment.
	if got := ProjectNameFor("/opt/./farrier/", b); got != want {
		t.Errorf("equivalent path derived %q, want %q", got, want)
	}
}

func TestProjectNameForSanitizesUnusualInput(t *testing.T) {
	for _, domain := range []string{
		"Git.Example.COM",
		"forge_under.example.com",
		"пример.example.com",
		"...",
		strings.Repeat("long", 40) + ".example.com",
		"",
	} {
		m := *testManifest()
		m.Domain = domain
		got := ProjectNameFor("/opt/farrier", &bundle.Bundle{Manifest: m})
		if !composeProjectName.MatchString(got) {
			t.Errorf("domain %q derived %q, which Compose will not accept", domain, got)
		}
	}
}

// TestPinProjectNameDerivesOnAFreshDirectory covers a remote directory no
// converge has run in: a new deployment, or a drill's scratch target.
func TestPinProjectNameDerivesOnAFreshDirectory(t *testing.T) {
	dir := t.TempDir()
	b := testBundle()

	if err := PinProjectName(context.Background(), shellTransport{}, dir, b); err != nil {
		t.Fatalf("PinProjectName: %v", err)
	}
	if got, want := readRecord(t, dir), ProjectNameFor(dir, b); got != want {
		t.Errorf("pinned %q, want the derived name %q", got, want)
	}
	if got := resolveProject(t, dir); got != ProjectNameFor(dir, b) {
		t.Errorf("resolved %q after pinning, want %q", got, ProjectNameFor(dir, b))
	}
}

// TestPinProjectNamePinsLegacyOnAnExistingDeployment is the other half of
// the migration. Shipped Compose files with no record beside them are a
// deployment an older binary made, and its containers answer to the old
// constant — upgrading the binary must not rename the project out from
// under them.
func TestPinProjectNamePinsLegacyOnAnExistingDeployment(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, composeDir), 0o755); err != nil {
		t.Fatalf("stage shipped compose files: %v", err)
	}

	if err := PinProjectName(context.Background(), shellTransport{}, dir, testBundle()); err != nil {
		t.Fatalf("PinProjectName: %v", err)
	}
	if got := readRecord(t, dir); got != LegacyProjectName {
		t.Errorf("pinned %q for an already-deployed directory, want %q", got, LegacyProjectName)
	}
}

// A record already there is never rewritten, whatever the bundle now says.
// `attach` (UP-007) fills a domain into a nameless bundle in place, on a
// host that is already running.
func TestPinProjectNameLeavesAnExistingRecordAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(ProjectPath(dir), []byte("farrier-already-pinned\n"), 0o644); err != nil {
		t.Fatalf("write record: %v", err)
	}

	if err := PinProjectName(context.Background(), shellTransport{}, dir, testBundle()); err != nil {
		t.Fatalf("PinProjectName: %v", err)
	}
	if got := readRecord(t, dir); got != "farrier-already-pinned" {
		t.Errorf("record became %q, want it left alone", got)
	}
}

// TestConvergePinsBeforeItRunsComposeUp is the ordering the whole scheme
// rests on: the project name has to be on the host before the command that
// creates the containers reads it.
func TestConvergePinsBeforeItRunsComposeUp(t *testing.T) {
	dir := t.TempDir()
	b := testBundle()
	transport := &recordingShellTransport{}

	if err := Converge(context.Background(), transport, dir, b); err != nil {
		t.Fatalf("Converge: %v", err)
	}
	if got, want := readRecord(t, dir), ProjectNameFor(dir, b); got != want {
		t.Errorf("converge pinned %q, want %q", got, want)
	}

	pin, up := -1, -1
	for i, cmd := range transport.commands {
		switch {
		case pin == -1 && strings.Contains(cmd, ProjectPath(dir)) && strings.Contains(cmd, "printf"):
			pin = i
		case strings.Contains(cmd, "docker compose up -d"):
			up = i
		}
	}
	if pin == -1 || up == -1 || pin > up {
		t.Fatalf("pin at %d, compose up at %d, in %v", pin, up, transport.commands)
	}
}

func readRecord(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(ProjectPath(dir))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	return strings.TrimSpace(string(raw))
}

// shellTransport runs commands in a real /bin/sh against the local
// filesystem. PinProjectName's whole decision is made in shell, so a fake
// that only records the command would be testing the string rather than
// what it does.
type shellTransport struct{}

func (shellTransport) Output(ctx context.Context, command string) ([]byte, error) {
	return exec.CommandContext(ctx, "/bin/sh", "-c", command).Output()
}

func (shellTransport) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, content, os.FileMode(mode))
}

func (shellTransport) Close() error { return nil }

// recordingShellTransport is shellTransport with the command log Converge's
// ordering test needs. `docker compose up` is not run — it is swallowed, so
// the test needs no Docker daemon — but everything before it is real.
type recordingShellTransport struct {
	shellTransport
	commands []string
}

func (r *recordingShellTransport) Output(ctx context.Context, command string) ([]byte, error) {
	r.commands = append(r.commands, command)
	if strings.Contains(command, "docker compose") {
		return nil, nil
	}
	return r.shellTransport.Output(ctx, command)
}
