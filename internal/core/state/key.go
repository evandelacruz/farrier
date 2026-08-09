package state

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// Key names for the bundle key material that isn't already named elsewhere.
// The three Forgejo secrets are forge.KeySecretKey, forge.KeyInternalToken,
// and forge.KeyLFSJWTSecret — KeyExporter reuses those names rather than
// declaring its own, so a name change in one place can't silently drift out
// of sync with the other.
const (
	KeyTLSCertificate = "tls_certificate"
	KeyTLSPrivateKey  = "tls_private_key"
	KeySSHHostKey     = "ssh_host_key"
	// KeySSHHostKeyPublic is KeySSHHostKey's public half, in
	// authorized-keys format — matches initialize.KeySSHHostKeyPublic's
	// value; declared separately here to keep state decoupled from
	// initialize (tech-spec.md "Repository layout"), the same reason
	// KeyTLSCertificate et al. are declared here rather than imported.
	KeySSHHostKeyPublic = "ssh_host_key.pub"
)

// keyNames is the fixed, ordered set every bundle's key material consists of
// (STATE-004, spec.md "Identity" > "Key material"): the three Forgejo
// secrets, the TLS certificate chain and its private key, and the SSH host
// key installed on every deploy so clients see an unchanged host identity. It
// is what Names enumerates, since a keystore.Driver (tech-spec "Keystore
// driver config": file, command, or an out-of-tree exec driver) only ever
// resolves a name it's given — none of the three shipped drivers can list
// what they hold. This must stay in sync with initialize.keyMaterialOrder
// (minus the age backup key, see below): a name here that init never stores
// makes every real backup fail on resolve, and a name init stores that isn't
// here is silently left out of every backup.
//
// The age backup key (spec.md "Key custody") is deliberately not in this
// set: it encrypts the backup, so it is never captured into one, and it is
// never installed onto a forge host — the operator holds it directly.
var keyNames = []string{
	forge.KeySecretKey,
	forge.KeyInternalToken,
	forge.KeyLFSJWTSecret,
	KeyTLSCertificate,
	KeyTLSPrivateKey,
	KeySSHHostKey,
	KeySSHHostKeyPublic,
}

// KeyExporter exposes a bundle's key material (STATE-004, spec.md "Identity"
// > "Key material") as an enumerable set of named secrets, each resolved
// through a keystore.Driver. Capture (backup, tech-spec "Snapshot format":
// keys/) walks Names and resolves each one into the snapshot; installation
// (restore, writing each secret back to wherever the target's keystore
// driver puts it) walks the same set on the way back in.
type KeyExporter interface {
	// Names returns every key name this bundle's key material consists of,
	// in a stable order.
	Names() []string

	// Resolve returns the secret named by one entry from Names.
	Resolve(ctx context.Context, name string) (keystore.Secret, error)
}

// KeystoreKeyExporter implements KeyExporter over a keystore.Driver: Names
// returns the fixed set every bundle carries, and Resolve reads one of them
// by name from Driver — the same lookup forge.ResolveSecrets and deploy.Up
// already perform for the three Forgejo secrets, generalized to the full
// set of key material a bundle carries.
type KeystoreKeyExporter struct {
	Driver keystore.Driver
}

// Names returns keyNames, the fixed set of key names every bundle carries.
func (e *KeystoreKeyExporter) Names() []string {
	names := make([]string, len(keyNames))
	copy(names, keyNames)
	return names
}

// Resolve reads name from Driver and returns its Secret unchanged — never
// logged, never written anywhere but the caller's own capture or
// installation path (KEY-003).
func (e *KeystoreKeyExporter) Resolve(ctx context.Context, name string) (keystore.Secret, error) {
	if e.Driver == nil {
		return keystore.Secret{}, errors.New("state: key exporter: driver is required")
	}
	if strings.TrimSpace(name) == "" {
		return keystore.Secret{}, errors.New("state: key exporter: name is required")
	}
	secret, err := e.Driver.Resolve(ctx, name)
	if err != nil {
		return keystore.Secret{}, fmt.Errorf("state: key exporter: resolve %s: %w", name, err)
	}
	return secret, nil
}

var _ KeyExporter = (*KeystoreKeyExporter)(nil)
