package initialize

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"fmt"

	"filippo.io/age"
	"golang.org/x/crypto/ssh"

	"github.com/evandelacruz/farrier/internal/core/acme"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// Key names for the bundle key material INIT-003 generates beyond
// Forgejo's own three (forge.KeySecretKey, forge.KeyInternalToken,
// forge.KeyLFSJWTSecret — owned by the forge package since forge is what
// consumes them). The TLS certificate and the SSH host key both install
// into the running service on every `up` (deploy.configureTLS,
// deploy.configureSSHHostKey — the latter RSTR-004), and the age key
// encrypts and decrypts snapshots (BKUP-003). Their names live here, next
// to the code that first generates them, until a consumer package claims
// them.
const (
	// KeyTLSCertificate is the PEM-encoded certificate chain issued during
	// zone-control proof (INIT-002) and persisted here as bundle identity.
	KeyTLSCertificate = "tls_certificate"
	// KeyTLSPrivateKey is the PEM-encoded private key for KeyTLSCertificate.
	KeyTLSPrivateKey = "tls_private_key"
	// KeySSHHostKey is the bundle's SSH host key — an ed25519 private key
	// in OpenSSH PEM format — so Forgejo's git-over-SSH server presents the
	// same identity on every host it runs on (RSTR-004).
	KeySSHHostKey = "ssh_host_key"
	// KeySSHHostKeyPublic is KeySSHHostKey's public half, in
	// authorized-keys format.
	KeySSHHostKeyPublic = "ssh_host_key.pub"
	// KeyAgeBackupKey is the age identity (spec.md "Backup encryption:
	// age.") backup and restore use to encrypt and decrypt snapshots
	// (BKUP-003). Only the identity is stored; its recipient (the public
	// half) is cheap to derive at the point it's needed.
	KeyAgeBackupKey = "age_backup_key"
)

// secretByteLength is the random byte length behind SECRET_KEY,
// INTERNAL_TOKEN, and the LFS JWT secret: 32 bytes, base64 (unpadded,
// URL-safe) encoded — enough entropy for Forgejo's HMAC/session use, and
// the same shape as Forgejo's own secret generator.
const secretByteLength = 32

// keyMaterialOrder fixes the order generateKeyMaterial's result is stored
// in, so a storage failure always names the same key first and Run's
// behavior is reproducible across repeated runs of the same failure.
var keyMaterialOrder = []string{
	forge.KeySecretKey,
	forge.KeyInternalToken,
	forge.KeyLFSJWTSecret,
	KeyTLSCertificate,
	KeyTLSPrivateKey,
	KeySSHHostKey,
	KeySSHHostKeyPublic,
	KeyAgeBackupKey,
}

// generateKeyMaterial produces every piece of key material INIT-003
// requires: Forgejo's SECRET_KEY, INTERNAL_TOKEN, and LFS JWT secret
// (random), the TLS certificate cert carries — the one obtained during
// zone-control proof's ACME DNS-01 exchange (INIT-002); persisting it here
// instead of discarding it and issuing a second one avoids a redundant
// exchange against the operator's ACME/DNS provider — a fresh SSH host key
// pair, and a fresh age backup key. It returns each as a keyName -> Secret
// pair, ready to hand to a keystore.Writer.
func generateKeyMaterial(cert *acme.Certificate) (map[string]keystore.Secret, error) {
	if cert == nil || len(cert.Certificate) == 0 || len(cert.PrivateKey) == 0 {
		return nil, fmt.Errorf("initialize: no certificate from zone-control proof to persist")
	}

	secretKey, err := randomSecret()
	if err != nil {
		return nil, fmt.Errorf("initialize: generate %s: %w", forge.KeySecretKey, err)
	}
	internalToken, err := randomSecret()
	if err != nil {
		return nil, fmt.Errorf("initialize: generate %s: %w", forge.KeyInternalToken, err)
	}
	lfsJWTSecret, err := randomSecret()
	if err != nil {
		return nil, fmt.Errorf("initialize: generate %s: %w", forge.KeyLFSJWTSecret, err)
	}
	sshPrivate, sshPublic, err := generateSSHHostKey()
	if err != nil {
		return nil, fmt.Errorf("initialize: generate %s: %w", KeySSHHostKey, err)
	}
	ageIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("initialize: generate %s: %w", KeyAgeBackupKey, err)
	}

	return map[string]keystore.Secret{
		forge.KeySecretKey:     keystore.NewSecret(secretKey),
		forge.KeyInternalToken: keystore.NewSecret(internalToken),
		forge.KeyLFSJWTSecret:  keystore.NewSecret(lfsJWTSecret),
		KeyTLSCertificate:      keystore.NewSecret(string(cert.Certificate)),
		KeyTLSPrivateKey:       keystore.NewSecret(string(cert.PrivateKey)),
		KeySSHHostKey:          keystore.NewSecret(sshPrivate),
		KeySSHHostKeyPublic:    keystore.NewSecret(sshPublic),
		KeyAgeBackupKey:        keystore.NewSecret(ageIdentity.String()),
	}, nil
}

// randomSecret returns secretByteLength cryptographically random bytes,
// base64 (unpadded, URL-safe) encoded — the shape SECRET_KEY,
// INTERNAL_TOKEN, and the LFS JWT secret share, since Forgejo treats all
// three as opaque strings.
func randomSecret() (string, error) {
	buf := make([]byte, secretByteLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// generateSSHHostKey generates a fresh ed25519 key pair for Forgejo's
// git-over-SSH server host identity, returning the private key in OpenSSH
// PEM format and the public key in authorized-keys format.
func generateSSHHostKey() (private, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", "", fmt.Errorf("marshal private key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", fmt.Errorf("derive public key: %w", err)
	}
	return string(pem.EncodeToMemory(block)), string(ssh.MarshalAuthorizedKey(sshPub)), nil
}

// storeKeyMaterial stores every key/secret pair in material through
// writer, in keyMaterialOrder, stopping at the first Store failure. Keys
// already written before the failure remain on disk, so a retried init
// hits the overwrite guard on the first of them — the operator must clear
// the keystore directory before re-running init.
func storeKeyMaterial(ctx context.Context, writer keystore.Writer, material map[string]keystore.Secret) error {
	for _, name := range keyMaterialOrder {
		secret, ok := material[name]
		if !ok {
			continue
		}
		if err := writer.Store(ctx, name, secret); err != nil {
			return fmt.Errorf("initialize: store %s: %w", name, err)
		}
	}
	return nil
}
