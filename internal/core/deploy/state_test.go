package deploy

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// composeText concatenates every file in compose, so a test can assert on
// mount strings without depending on which filename Render happened to use.
func composeText(compose map[string][]byte) string {
	var b strings.Builder
	for _, v := range compose {
		b.Write(v)
	}
	return b.String()
}

func TestStatePathHelpers(t *testing.T) {
	if got, want := GitStatePath("/opt/farrier"), "/opt/farrier/state/git"; got != want {
		t.Errorf("GitStatePath = %q, want %q", got, want)
	}
	if got, want := GiteaStatePath("/opt/farrier"), "/opt/farrier/state/gitea"; got != want {
		t.Errorf("GiteaStatePath = %q, want %q", got, want)
	}
	if got, want := BlobsStatePath("/opt/farrier"), "/opt/farrier/state/blobs"; got != want {
		t.Errorf("BlobsStatePath = %q, want %q", got, want)
	}
}

func TestChownStateChownsBothPathsRecursively(t *testing.T) {
	host := newFakeHost()
	if err := ChownState(context.Background(), host, "/opt/farrier"); err != nil {
		t.Fatalf("ChownState: %v", err)
	}

	var saw bool
	wantOwner := fmt.Sprintf("chown -R %d:%d", forgeUID, forgeGID)
	for _, cmd := range host.commands {
		if strings.Contains(cmd, wantOwner) &&
			strings.Contains(cmd, "/opt/farrier/state/git") &&
			strings.Contains(cmd, "/opt/farrier/state/gitea") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("no recursive chown command for both state directories, commands: %v", host.commands)
	}
}

func TestConfigureStateMountsGitAndGiteaOwnedByForgeUser(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)

	compose, owned, err := configureState(context.Background(), host, b, "/opt/farrier", b.Compose)
	if err != nil {
		t.Fatalf("configureState: %v", err)
	}
	if !owned {
		t.Error("configureState reported it could not set ownership, want it reporting the chown applied")
	}

	composed := composeText(compose)
	if !strings.Contains(composed, "/opt/farrier/state/git:"+forge.RepoRoot) {
		t.Errorf("compose missing git state mount: %s", composed)
	}
	if !strings.Contains(composed, "/opt/farrier/state/gitea:"+forge.DataPath) {
		t.Errorf("compose missing gitea state mount: %s", composed)
	}

	var sawCreate, sawOwnership bool
	wantOwner := fmt.Sprintf("chown %d:%d", forgeUID, forgeGID)
	for _, cmd := range host.commands {
		if strings.Contains(cmd, "mkdir -p") && strings.Contains(cmd, "/opt/farrier/state/git") &&
			strings.Contains(cmd, "/opt/farrier/state/gitea") {
			sawCreate = true
		}
		if strings.Contains(cmd, wantOwner) && strings.Contains(cmd, "/opt/farrier/state/git") &&
			strings.Contains(cmd, "/opt/farrier/state/gitea") {
			sawOwnership = true
		}
	}
	if !sawCreate {
		t.Errorf("no command created both state directories, commands: %v", host.commands)
	}
	if !sawOwnership {
		t.Errorf("no command chowned both state directories, commands: %v", host.commands)
	}
}

func TestConfigureStateCreatesBlobsDirWhenBlobDriverIsLocal(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	b.Manifest.Drivers.Blob = bundle.DriverRef{Driver: "local", Config: map[string]any{"path": "testdata/blobs"}}

	if _, _, err := configureState(context.Background(), host, b, "/opt/farrier", b.Compose); err != nil {
		t.Fatalf("configureState: %v", err)
	}

	var sawBlobsDir bool
	for _, cmd := range host.commands {
		if strings.Contains(cmd, "/opt/farrier/state/blobs") {
			if !strings.Contains(cmd, "mkdir -p") {
				t.Errorf("command referencing the blobs directory isn't a plain mkdir: %q", cmd)
			}
			if strings.Contains(cmd, "chown") {
				t.Errorf("blobs directory was chowned to the forgejo container user, want it left alone: %q", cmd)
			}
			sawBlobsDir = true
		}
	}
	if !sawBlobsDir {
		t.Errorf("no command created the blobs state directory, commands: %v", host.commands)
	}
}

func TestConfigureStateSkipsBlobsDirForNonLocalBlobDriver(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	b.Manifest.Drivers.Blob = bundle.DriverRef{Driver: "s3", Config: map[string]any{
		"bucket": "farrier", "endpoint": "s3.example.com",
	}}

	if _, _, err := configureState(context.Background(), host, b, "/opt/farrier", b.Compose); err != nil {
		t.Fatalf("configureState: %v", err)
	}

	for _, cmd := range host.commands {
		if strings.Contains(cmd, "/opt/farrier/state/blobs") {
			t.Errorf("command referenced the blobs state directory for a non-local blob driver: %q", cmd)
		}
	}
}

// TestUpMountsStateAndCreatesDirsBeforeConverge exercises UP-004 end to end
// through Up: state directories must exist, owned by the forgejo user,
// before docker compose up ever runs, or Docker would create them
// root-owned and Forgejo could never write to them.
func TestUpMountsStateAndCreatesDirsBeforeConverge(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()

	if err := Up(context.Background(), job, host, testBundle(t), testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	composePath := "/opt/farrier/compose.tmp/" + orchestrate.ComposeFile
	compose, ok := host.files[composePath]
	if !ok {
		t.Fatalf("compose file not shipped under %s, wrote: %v", composePath, keysOf(host.files))
	}
	if !strings.Contains(compose, "/opt/farrier/state/git:"+forge.RepoRoot) {
		t.Errorf("shipped compose missing git state mount: %s", compose)
	}
	if !strings.Contains(compose, "/opt/farrier/state/gitea:"+forge.DataPath) {
		t.Errorf("shipped compose missing gitea state mount: %s", compose)
	}

	mkdirIdx, composeUpIdx := -1, -1
	for i, cmd := range host.commands {
		if strings.Contains(cmd, "mkdir -p") && strings.Contains(cmd, "/opt/farrier/state/git") && mkdirIdx == -1 {
			mkdirIdx = i
		}
		if strings.Contains(cmd, "docker compose up -d") && composeUpIdx == -1 {
			composeUpIdx = i
		}
	}
	if mkdirIdx == -1 {
		t.Fatalf("state directories never created, commands: %v", host.commands)
	}
	if composeUpIdx == -1 {
		t.Fatalf("docker compose up never ran, commands: %v", host.commands)
	}
	if mkdirIdx > composeUpIdx {
		t.Errorf("state directories created (index %d) after docker compose up (index %d)", mkdirIdx, composeUpIdx)
	}
}
