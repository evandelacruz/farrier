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
// known_hosts entry for the endpoint UP-005 publishes.
//
// The key comes from the manifest, which `init` fills in from the same
// keystore entry deploy.Up installs on the host — so the key publish pins
// is by construction the key the endpoint presents, on a first deploy and
// on a restored or promoted instance alike (RSTR-004). Reading it from the
// manifest rather than the keystore is what lets someone publish to an
// instance they do not hold the secrets for: a public host key is not a
// secret, and requiring keystore access to read one meant requiring access
// to SECRET_KEY and the age backup key alongside it.
func knownHostsLine(ctx context.Context, m *bundle.Manifest) (string, error) {
	public, source, err := hostPublicKey(ctx, m)
	if err != nil {
		return "", err
	}
	line, err := m.SSHKnownHostsLineFor(public)
	if err != nil {
		return "", fmt.Errorf("%s: %w", source, err)
	}
	return line, nil
}

// hostPublicKey returns the instance's SSH host public key and a phrase
// naming where it came from, for an error to attribute a bad value to.
//
// A manifest written before the field existed falls back to the bundle's
// keystore, which is where the key has always been. Falling back rather
// than refusing keeps every bundle already on disk publishing exactly as it
// did — under the same pin, from the same value — and leaves the new field
// as the thing that removes a requirement rather than one that adds one.
// The fallback is deliberately not a silent skip: a bundle with no key in
// either place fails the push instead of accepting whatever host answers.
func hostPublicKey(ctx context.Context, m *bundle.Manifest) (key, source string, err error) {
	if manifestKey := strings.TrimSpace(m.SSHHostKeyPublic); manifestKey != "" {
		return manifestKey, "the bundle manifest's ssh host public key", nil
	}

	driver, err := keystore.New(m.Drivers.Keystore.Driver, m.Drivers.Keystore.Config)
	if err != nil {
		return "", "", fmt.Errorf("keystore driver: %w", err)
	}
	secret, err := driver.Resolve(ctx, state.KeySSHHostKeyPublic)
	if err != nil {
		return "", "", fmt.Errorf("this bundle predates the manifest's ssh host public key, so it was read from the keystore instead: resolve %s: %w", state.KeySSHHostKeyPublic, err)
	}
	return secret.Reveal(), state.KeySSHHostKeyPublic + " from the bundle's keystore", nil
}
