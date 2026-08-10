package deploy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
)

// The probe scripts are run here by a real /bin/sh against real
// directories, rather than asserted against as strings. What they promise
// — that a directory the forge cannot use is detected, and that nothing is
// left behind either way — is a property of what the shell does with them,
// and a test that only matched substrings would keep passing through a
// rewrite that broke it.

// runProbeScript runs script the way `docker run --entrypoint /bin/sh ...
// -c` does, returning its stderr and whether it exited zero.
func runProbeScript(t *testing.T, script string) (string, error) {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Stderr = &stderr
	err := cmd.Run()
	return strings.TrimSpace(stderr.String()), err
}

// assertEmptyDir fails unless dir contains nothing at all — the probe file
// included, which is the point.
func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("probe left files behind in %s: %v", dir, names)
	}
}

func TestStateProbeScriptPassesOnUsableDirectoriesAndCleansUp(t *testing.T) {
	git, gitea := t.TempDir(), t.TempDir()

	stderr, err := runProbeScript(t, stateProbeScript([]probeDir{
		{container: git, host: "/opt/farrier/state/git"},
		{container: gitea, host: "/opt/farrier/state/gitea"},
	}))
	if err != nil {
		t.Fatalf("probe failed against writable directories: %v (stderr: %s)", err, stderr)
	}
	if stderr != "" {
		t.Errorf("probe wrote to stderr on success: %q", stderr)
	}
	assertEmptyDir(t, git)
	assertEmptyDir(t, gitea)
}

// TestStateProbeScriptFailsOnUnusableDirectoryAndCleansUp is the Linux
// non-root case the change must not turn into a silent success: a state
// directory the forge genuinely cannot write to fails the probe, and says
// which host directory it was. The unusable path here is a regular file
// rather than a mode-stripped directory so the case is the same for root,
// who would otherwise write through any mode the test set.
func TestStateProbeScriptFailsOnUnusableDirectoryAndCleansUp(t *testing.T) {
	git := t.TempDir()
	notADir := filepath.Join(t.TempDir(), "gitea")
	if err := os.WriteFile(notADir, nil, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	stderr, err := runProbeScript(t, stateProbeScript([]probeDir{
		{container: git, host: "/opt/farrier/state/git"},
		{container: notADir, host: "/opt/farrier/state/gitea"},
	}))
	if err == nil {
		t.Fatal("probe passed against a directory it cannot write to, want failure")
	}
	if stderr != "/opt/farrier/state/gitea" {
		t.Errorf("probe named %q as the failing directory, want the host path of the second one", stderr)
	}
	assertEmptyDir(t, git)
}

// TestStateProbeScriptFailsWhenReadBackIsWrong covers the mount that
// accepts a write and serves something else back: the probe reads what it
// wrote, so that is a failure rather than a pass. The stand-in is a
// directory where the probe's own filename is already a subdirectory, which
// no write can turn into the expected content.
func TestStateProbeScriptFailsWhenReadBackIsWrong(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, accessProbeFile), 0o755); err != nil {
		t.Fatalf("make fixture: %v", err)
	}

	if _, err := runProbeScript(t, stateProbeScript([]probeDir{
		{container: dir, host: "/opt/farrier/state/git"},
	})); err == nil {
		t.Fatal("probe passed without reading back what it wrote, want failure")
	}
}

// TestFileProbeScriptPassesOnUsableFileAndLeavesItAlone covers the
// database restore places: the probe must clear it, and must hand it back
// byte-for-byte as it found it.
func TestFileProbeScriptPassesOnUsableFileAndLeavesItAlone(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "gitea.db")
	content := []byte("SQLite format 3\x00not really")
	if err := os.WriteFile(db, content, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	stderr, err := runProbeScript(t, fileProbeScript([]probeDir{
		{container: db, host: "/opt/farrier/state/gitea/gitea.db"},
	}))
	if err != nil {
		t.Fatalf("probe failed against a usable file: %v (stderr: %s)", err, stderr)
	}
	if stderr != "" {
		t.Errorf("probe wrote to stderr on success: %q", stderr)
	}

	got, err := os.ReadFile(db)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("probe changed the file: %q, want %q", got, content)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Errorf("probe left extra entries in %s: %v (err %v)", dir, entries, err)
	}
}

// TestFileProbeScriptFailsOnMissingFileWithoutCreatingIt is the guard in
// front of the append: an absent database must fail the probe, not be
// conjured into an empty one that passes it.
func TestFileProbeScriptFailsOnMissingFileWithoutCreatingIt(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "gitea.db")

	stderr, err := runProbeScript(t, fileProbeScript([]probeDir{
		{container: missing, host: "/opt/farrier/state/gitea/gitea.db"},
	}))
	if err == nil {
		t.Fatal("probe passed against a file that is not there, want failure")
	}
	if stderr != "/opt/farrier/state/gitea/gitea.db" {
		t.Errorf("probe named %q, want the host path of the file", stderr)
	}
	assertEmptyDir(t, dir)
}

// TestFileProbeScriptFailsOnUnusableFile is the Linux non-root case for a
// placed file. The stand-in is a directory where the file should be, so
// the case is the same for root, who would write straight through any mode
// the test set.
func TestFileProbeScriptFailsOnUnusableFile(t *testing.T) {
	stderr, err := runProbeScript(t, fileProbeScript([]probeDir{
		{container: t.TempDir(), host: "/opt/farrier/state/gitea/gitea.db"},
	}))
	if err == nil {
		t.Fatal("probe passed against a path it cannot open as a file, want failure")
	}
	if stderr != "/opt/farrier/state/gitea/gitea.db" {
		t.Errorf("probe named %q, want the host path of the file", stderr)
	}
}

// TestPlacedStateProbeScriptFailsOnTheRestoredDirectoryAndCleansUp runs the
// whole script VerifyForgeCanUsePlacedState sends — directories and files
// together — against a restored repository directory the forge cannot
// write. This is the case that used to fail loudly at the recursive chown
// and must not become a silent success: the state directory above it is
// perfectly usable, and the probe still refuses, naming the directory that
// is not.
func TestPlacedStateProbeScriptFailsOnTheRestoredDirectoryAndCleansUp(t *testing.T) {
	usable := t.TempDir()
	unwritable := filepath.Join(t.TempDir(), "widgets.git")
	if err := os.WriteFile(unwritable, nil, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	db := filepath.Join(t.TempDir(), "gitea.db")
	if err := os.WriteFile(db, []byte("db"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	script := stateProbeScript([]probeDir{
		{container: usable, host: "/opt/farrier/state/git/acme/gadgets.git"},
		{container: unwritable, host: "/opt/farrier/state/git/acme/widgets.git"},
	}) + "; " + fileProbeScript([]probeDir{
		{container: db, host: "/opt/farrier/state/gitea/gitea.db"},
	})

	stderr, err := runProbeScript(t, script)
	if err == nil {
		t.Fatal("probe passed against a restored repository the forge cannot write, want failure")
	}
	if stderr != "/opt/farrier/state/git/acme/widgets.git" {
		t.Errorf("probe named %q, want the host path of the restored repository", stderr)
	}
	assertEmptyDir(t, usable)
}

// TestVerifyForgeCanUsePlacedStateProbesWhatRestorePlaced pins the whole
// point of the second check: the paths it exercises are the restored
// repository directories and the database file, at their real container
// locations under the deployment's own mounts — not the top of the state
// directories `up` already covers.
func TestVerifyForgeCanUsePlacedStateProbesWhatRestorePlaced(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	image := b.Manifest.Images[forge.Service]

	err := VerifyForgeCanUsePlacedState(context.Background(), host, image, "/opt/farrier",
		[]string{"/opt/farrier/state/git/acme/widgets.git"},
		[]string{"/opt/farrier/state/gitea/gitea.db"})
	if err != nil {
		t.Fatalf("VerifyForgeCanUsePlacedState: %v", err)
	}
	if len(host.commands) != 1 {
		t.Fatalf("got %d commands, want 1: %v", len(host.commands), host.commands)
	}

	cmd := host.commands[0]
	for _, want := range []string{
		"docker run",
		fmt.Sprintf("-u %d:%d", forgeUID, forgeGID),
		"'/opt/farrier/state/git':" + forge.RepoRoot,
		"'/opt/farrier/state/gitea':" + forge.DataPath,
		image,
		// The script is one shell-quoted argument, so its own quoting is
		// doubled by the time it reaches the command line.
		`probe '\''` + forge.RepoRoot + `/acme/widgets.git'\''`,
		`probefile '\''` + forge.DataPath + `/gitea.db'\''`,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("probe command missing %q: %s", want, cmd)
		}
	}
}

// TestVerifyForgeCanUsePlacedStateFailureIsActionable holds the restore
// failure to what an operator can do with it: that the forge's own uid was
// denied, which paths were checked, and the two ways out.
func TestVerifyForgeCanUsePlacedStateFailureIsActionable(t *testing.T) {
	host := newFakeHost()
	host.failOutputOn = "docker run"

	err := VerifyForgeCanUsePlacedState(context.Background(), host, "forgejo@sha256:x", "/opt/farrier",
		[]string{"/opt/farrier/state/git/acme/widgets.git"},
		[]string{"/opt/farrier/state/gitea/gitea.db"})
	if err == nil {
		t.Fatal("VerifyForgeCanUsePlacedState: want error when the forge cannot use restored state, got nil")
	}
	for _, want := range []string{
		"/opt/farrier/state/git/acme/widgets.git",
		"/opt/farrier/state/gitea/gitea.db",
		fmt.Sprintf("uid %d", forgeUID),
		fmt.Sprintf("%d:%d", forgeUID, forgeGID),
		"/opt/farrier/state",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestVerifyForgeCanUsePlacedStateRefusesPathsItCannotSee guards the
// mapping: a path outside the two mounted directories cannot be probed at
// all, and saying so is better than mounting something else or probing
// nothing and passing.
func TestVerifyForgeCanUsePlacedStateRefusesPathsItCannotSee(t *testing.T) {
	host := newFakeHost()

	err := VerifyForgeCanUsePlacedState(context.Background(), host, "forgejo@sha256:x", "/opt/farrier",
		[]string{"/var/lib/elsewhere"}, nil)
	if err == nil {
		t.Fatal("VerifyForgeCanUsePlacedState: want error for a path outside the state directories, got nil")
	}
	if len(host.commands) != 0 {
		t.Errorf("host was touched despite the path being unprobeable: %v", host.commands)
	}
}

func TestSummarizePathsKeepsTheMessageReadable(t *testing.T) {
	if got := summarizePaths([]string{"a", "b"}); got != "a, b" {
		t.Errorf("summarizePaths(2) = %q", got)
	}
	if got := summarizePaths([]string{"a", "b", "c", "d", "e"}); got != "a, b, c and 2 more" {
		t.Errorf("summarizePaths(5) = %q", got)
	}
}

func TestReadProbeScript(t *testing.T) {
	present := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(present, []byte("key material"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := runProbeScript(t, readProbeScript(probeDir{container: present, host: "/opt/farrier/state/gitea/ssh/key"})); err != nil {
		t.Errorf("read probe failed against a readable file: %v", err)
	}

	missing := filepath.Join(t.TempDir(), "key")
	stderr, err := runProbeScript(t, readProbeScript(probeDir{container: missing, host: "/opt/farrier/state/gitea/ssh/key"}))
	if err == nil {
		t.Fatal("read probe passed against an unreadable file, want failure")
	}
	if stderr != "/opt/farrier/state/gitea/ssh/key" {
		t.Errorf("read probe named %q, want the host path of the file", stderr)
	}
}

// TestVerifyForgeCanUseStateRunsAsTheForgeUserOverTheRealMounts pins what
// makes the check meaningful: it runs in the bundle's own pinned forgejo
// image, as the uid the forge runs as, with the host's state directories
// mounted at the container paths app.ini names. Checking ownership or mode
// bits from the host instead would answer a different question — and answer
// it wrong on a host whose container runtime maps ownership.
func TestVerifyForgeCanUseStateRunsAsTheForgeUserOverTheRealMounts(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	image := b.Manifest.Images[forge.Service]

	if err := verifyForgeCanUseState(context.Background(), host, image, "/opt/farrier"); err != nil {
		t.Fatalf("verifyForgeCanUseState: %v", err)
	}
	if len(host.commands) != 1 {
		t.Fatalf("got %d commands, want 1: %v", len(host.commands), host.commands)
	}

	cmd := host.commands[0]
	for _, want := range []string{
		"docker run",
		fmt.Sprintf("-u %d:%d", forgeUID, forgeGID),
		"'/opt/farrier/state/git':" + forge.RepoRoot,
		"'/opt/farrier/state/gitea':" + forge.DataPath,
		image,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("probe command missing %q: %s", want, cmd)
		}
	}
}

// TestVerifyForgeCanUseStateFailureIsActionable holds the failure to what
// an operator can do with it: which directories, that the forge's own uid
// is the one denied, and the two ways out. The raw "chown: Operation not
// permitted" this replaced told them none of it.
func TestVerifyForgeCanUseStateFailureIsActionable(t *testing.T) {
	host := newFakeHost()
	host.failOutputOn = "docker run"

	err := verifyForgeCanUseState(context.Background(), host, "forgejo@sha256:x", "/opt/farrier")
	if err == nil {
		t.Fatal("verifyForgeCanUseState: want error when the forge cannot write, got nil")
	}
	for _, want := range []string{
		"/opt/farrier/state/git",
		"/opt/farrier/state/gitea",
		fmt.Sprintf("uid %d", forgeUID),
		fmt.Sprintf("%d:%d", forgeUID, forgeGID),
		"`up`",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestConfigureStateSucceedsWhenOwnershipCannotBeSet is the macOS case and
// the whole point of the change: the chown is refused, the forge can read
// and write its state anyway, and `up` continues — reporting that ownership
// was not set rather than failing on it.
func TestConfigureStateSucceedsWhenOwnershipCannotBeSet(t *testing.T) {
	host := newFakeHost()
	host.failOutputOn = "chown"
	b := testBundle(t)

	_, owned, err := configureState(context.Background(), host, b, "/opt/farrier", b.Compose)
	if err != nil {
		t.Fatalf("configureState: %v", err)
	}
	if owned {
		t.Error("configureState reported ownership was set, want it reporting the refused chown")
	}

	var probed bool
	for _, cmd := range host.commands {
		if strings.Contains(cmd, "docker run") && strings.Contains(cmd, accessProbeFile) {
			probed = true
		}
	}
	if !probed {
		t.Errorf("no access probe ran, commands: %v", host.commands)
	}
}

// TestConfigureStateFailsWhenForgeCannotUseState is the other half: a host
// where the forge really cannot write must still fail, or the change would
// have traded a loud breakage for a silent one.
func TestConfigureStateFailsWhenForgeCannotUseState(t *testing.T) {
	host := newFakeHost()
	host.failOutputOn = "docker run"
	b := testBundle(t)

	if _, _, err := configureState(context.Background(), host, b, "/opt/farrier", b.Compose); err == nil {
		t.Fatal("configureState: want error when the forge cannot use its state, got nil")
	}
}

// TestUpSucceedsWhenOwnershipCannotBeSet runs the macOS case through Up
// end to end: every chown in the deployment is refused — the state
// directories and the SSH host key directory alike — and the deployment
// completes, because what it verifies is access rather than ownership. This
// is `farrier up -target ssh://user@localhost` on a laptop (ORCH-003,
// UP-006), which failed outright before.
func TestUpSucceedsWhenOwnershipCannotBeSet(t *testing.T) {
	host := newFakeHost()
	host.failOutputOn = "chown"
	job := events.NewJob()

	if err := Up(context.Background(), job, host, testBundle(t), testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}
}

// TestUpFailsWhenForgeCannotUseState is the same deployment on a host that
// genuinely denies the forge its state: Up stops at configure-state rather
// than converging and leaving Forgejo to fail on its own later.
func TestUpFailsWhenForgeCannotUseState(t *testing.T) {
	host := newFakeHost()
	host.failOutputOn = "docker run --rm --network none"
	job := events.NewJob()

	err := Up(context.Background(), job, host, testBundle(t), testOptions("/opt/farrier"))
	if err == nil {
		t.Fatal("Up: want error when the forge cannot use its state, got nil")
	}
	if !strings.Contains(err.Error(), "cannot read and write its state") {
		t.Errorf("Up failed with %v, want the state-access failure", err)
	}
	for _, cmd := range host.commands {
		if strings.Contains(cmd, "docker compose up") {
			t.Errorf("converged despite the forge being unable to use its state: %q", cmd)
		}
	}
}
