package orchestrate

import (
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
	if !strings.Contains(cmd, "COMPOSE_PROJECT_NAME="+ProjectName) {
		t.Errorf("command = %q, want COMPOSE_PROJECT_NAME=%s", cmd, ProjectName)
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
