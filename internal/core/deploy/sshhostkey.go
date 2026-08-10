package deploy

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// sshHostKeyRelPath is forge.SSHHostKeyPath's location relative to
// forge.DataPath — "ssh/farrier_host_ed25519" today — derived rather than
// hardcoded a second time, the same reason restore's databaseRelPath
// derives from forge.DatabasePath instead of repeating "gitea.db".
func sshHostKeyRelPath() string {
	return strings.TrimPrefix(forge.SSHHostKeyPath, forge.DataPath+"/")
}

// configureSSHHostKey resolves the bundle's persisted ed25519 SSH host key
// (state.KeySSHHostKey, state.KeySSHHostKeyPublic — INIT-003) and writes it
// into GiteaStatePath at the location forge.RenderAppINI configures as
// Forgejo's SSH_SERVER_HOST_KEYS (forge.SSHHostKeyPath) — the same
// directory configureState already bind-mounts to forge.DataPath — so
// Forgejo's git-over-SSH server loads the bundle's own key on every boot
// instead of generating a fresh one the first time it finds none there
// (RSTR-004, spec.md "Identity" > "Key material": SSH host keys install on
// every deploy including the first, so a fresh host and a restored one both
// present an unchanged identity).
//
// This runs on every Up — an ordinary `up` on the instance's original host
// and a `restore` onto a fresh one alike — the same way configureTLS
// reuses the persisted certificate on every run rather than only at
// restore. A re-run against a host that's already serving this key writes
// the identical bytes back, a no-op in effect (UP-003); a host that
// doesn't have it yet — a first `up`, or a `restore` target with an empty
// data directory — gets the bundle's key instead of whatever Forgejo would
// otherwise generate for itself. That keeps the key every future backup
// captures (state.KeystoreKeyExporter reads the keystore, not the live
// host) the one Forgejo is actually presenting, and is what makes
// restore.Restore's own installKeys step — which only ever writes the
// keystore, never the running service — actually reach clients on the
// next `up` it triggers.
//
// Ownership matters here the same way it does for restore's own
// ChownState: the SSH session writing these files is generally not the
// uid:gid the forgejo container runs as, and a 0600 private key file left
// owned by that session would be unreadable to the container. So the chown
// is attempted, and — because it is one mechanism for that rather than the
// thing itself, and is refused outright on a host whose container runtime
// maps ownership instead (access.go) — what this fails on is the forge
// being unable to read the key, not the chown being unable to run.
func configureSSHHostKey(ctx context.Context, host Host, b *bundle.Bundle, remoteDir string) error {
	driver, err := keystore.New(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		return fmt.Errorf("keystore driver: %w", err)
	}

	private, err := driver.Resolve(ctx, state.KeySSHHostKey)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", state.KeySSHHostKey, err)
	}
	public, err := driver.Resolve(ctx, state.KeySSHHostKeyPublic)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", state.KeySSHHostKeyPublic, err)
	}

	keyPath := path.Join(GiteaStatePath(remoteDir), sshHostKeyRelPath())
	pubPath := keyPath + ".pub"

	if err := host.WriteFile(ctx, keyPath, []byte(private.Reveal()), 0o600); err != nil {
		return fmt.Errorf("ship ssh host key: %w", err)
	}
	if err := host.WriteFile(ctx, pubPath, []byte(public.Reveal()), 0o644); err != nil {
		return fmt.Errorf("ship ssh host key public half: %w", err)
	}

	chownBestEffort(ctx, host, true, path.Dir(keyPath))
	return verifyForgeCanReadSSHHostKey(ctx, host, b.Manifest.Images[forge.Service], remoteDir)
}
