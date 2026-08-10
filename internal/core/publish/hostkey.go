package publish

import (
	"context"
	"fmt"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// knownHostsLine renders the instance's SSH host public key as a
// known_hosts entry for the endpoint UP-005 publishes: the manifest's
// git-over-SSH host spelling, then the key's type and blob.
//
// The key comes from the bundle's own keystore, which is where INIT-003
// generated it and where deploy.Up reads it from to install on the host —
// so the key publish pins is by construction the key the endpoint
// presents, on a first deploy and on a restored or promoted instance
// alike (RSTR-004).
//
// The comment field of the stored authorized-keys line is dropped: it is
// not part of a known_hosts entry, and OpenSSH would read whatever follows
// the blob as a further host-key option.
func knownHostsLine(ctx context.Context, m *bundle.Manifest) (string, error) {
	driver, err := keystore.New(m.Drivers.Keystore.Driver, m.Drivers.Keystore.Config)
	if err != nil {
		return "", fmt.Errorf("keystore driver: %w", err)
	}
	secret, err := driver.Resolve(ctx, state.KeySSHHostKeyPublic)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", state.KeySSHHostKeyPublic, err)
	}

	keyType, blob, err := splitPublicKey(secret.Reveal())
	if err != nil {
		return "", fmt.Errorf("%s: %w", state.KeySSHHostKeyPublic, err)
	}
	return fmt.Sprintf("%s %s %s\n", m.GitSSHKnownHostsHost(), keyType, blob), nil
}

// splitPublicKey pulls the type and base64 blob out of an OpenSSH
// authorized-keys line, discarding any comment. It reports the shape it
// wanted rather than the bytes it got: a host key is public, but nothing
// resolved out of the keystore is ever echoed into an error, an event, or
// a log (KEY-003).
func splitPublicKey(line string) (keyType, blob string, err error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		return "", "", fmt.Errorf("is not an openssh public key: want \"<type> <base64> [comment]\"")
	}
	return fields[0], fields[1], nil
}
