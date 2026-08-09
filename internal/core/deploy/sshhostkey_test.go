package deploy

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/forge"
)

// TestConfigureSSHHostKeyShipsPersistedKeyUnderGiteaState exercises
// RSTR-004's core: the bundle's own persisted ed25519 SSH host key — not a
// fresh one Forgejo would otherwise generate — lands exactly where
// forge.RenderAppINI's SSH_SERVER_HOST_KEYS points, inside the directory
// configureState already bind-mounts to forge.DataPath.
func TestConfigureSSHHostKeyShipsPersistedKeyUnderGiteaState(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)

	if err := configureSSHHostKey(context.Background(), host, b, "/opt/farrier"); err != nil {
		t.Fatalf("configureSSHHostKey: %v", err)
	}

	wantRel := strings.TrimPrefix(forge.SSHHostKeyPath, forge.DataPath+"/")
	keyPath := "/opt/farrier/state/gitea/" + wantRel

	privateFixture, err := os.ReadFile(filepath.Join("testdata", "keys", "ssh_host_key"))
	if err != nil {
		t.Fatalf("read fixture private key: %v", err)
	}
	publicFixture, err := os.ReadFile(filepath.Join("testdata", "keys", "ssh_host_key.pub"))
	if err != nil {
		t.Fatalf("read fixture public key: %v", err)
	}

	if got := host.files[keyPath]; got != string(privateFixture) {
		t.Errorf("shipped private key at %s = %q, want %q", keyPath, got, string(privateFixture))
	}
	if got := host.files[keyPath+".pub"]; got != string(publicFixture) {
		t.Errorf("shipped public key at %s = %q, want %q", keyPath+".pub", got, string(publicFixture))
	}
}

// TestConfigureSSHHostKeyChownsToForgeUser guards the same failure mode
// ChownState exists for: the SSH session writing these files is not the
// uid:gid the forgejo container runs as, so a private key left owned by
// that session would be unreadable inside the container.
func TestConfigureSSHHostKeyChownsToForgeUser(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)

	if err := configureSSHHostKey(context.Background(), host, b, "/opt/farrier"); err != nil {
		t.Fatalf("configureSSHHostKey: %v", err)
	}

	wantOwner := fmt.Sprintf("chown -R %d:%d", forgeUID, forgeGID)
	wantRel := strings.TrimPrefix(forge.SSHHostKeyPath, forge.DataPath+"/")
	wantDir := path.Dir("/opt/farrier/state/gitea/" + wantRel)
	var saw bool
	for _, cmd := range host.commands {
		if strings.Contains(cmd, wantOwner) && strings.Contains(cmd, wantDir) {
			saw = true
		}
	}
	if !saw {
		t.Errorf("no recursive chown command for the ssh host key directory, commands: %v", host.commands)
	}
}

// TestConfigureSSHHostKeyFailsWhenKeystoreMissingKey guards against
// silently deploying without the bundle's identity: a keystore that can't
// resolve the SSH host key must fail configureSSHHostKey rather than let
// Forgejo generate an unmanaged one that never gets backed up.
func TestConfigureSSHHostKeyFailsWhenKeystoreMissingKey(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	b.Manifest.Drivers.Keystore.Config = map[string]any{"path": t.TempDir()}

	if err := configureSSHHostKey(context.Background(), host, b, "/opt/farrier"); err == nil {
		t.Fatal("configureSSHHostKey: want error against an empty keystore, got nil")
	}
}

// TestConfigureSSHHostKeyIsIdempotent exercises UP-003: running it twice
// against a host already carrying the key writes the identical bytes back
// rather than failing or drifting.
func TestConfigureSSHHostKeyIsIdempotent(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)

	if err := configureSSHHostKey(context.Background(), host, b, "/opt/farrier"); err != nil {
		t.Fatalf("configureSSHHostKey (first): %v", err)
	}
	first := make(map[string]string, len(host.files))
	for k, v := range host.files {
		first[k] = v
	}

	if err := configureSSHHostKey(context.Background(), host, b, "/opt/farrier"); err != nil {
		t.Fatalf("configureSSHHostKey (second): %v", err)
	}
	for k, v := range first {
		if host.files[k] != v {
			t.Errorf("file %s changed on re-run: %q -> %q", k, v, host.files[k])
		}
	}
}
