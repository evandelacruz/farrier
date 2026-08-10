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
	"github.com/evandelacruz/farrier/internal/core/events"
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
	forge.KeyRunnerSecret,
	KeyTLSCertificate,
	KeyTLSPrivateKey,
	KeySSHHostKey,
	KeySSHHostKeyPublic,
	KeyAgeBackupKey,
}

// generateKeyMaterial produces every piece of key material INIT-003
// requires: Forgejo's SECRET_KEY, INTERNAL_TOKEN, and LFS JWT secret
// (random), the colocated Actions runner's registration secret (FORGE-005),
// the TLS certificate cert carries — the one obtained during
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
	// Generated through forge rather than randomSecret: Forgejo's offline
	// runner registration reads the first 16 of the secret's 40 hex
	// characters as the runner's identifier, so its format is fixed by
	// Forgejo and belongs next to the code that uses it (FORGE-005).
	runnerSecret, err := forge.NewRunnerSecret()
	if err != nil {
		return nil, fmt.Errorf("initialize: generate %s: %w", forge.KeyRunnerSecret, err)
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
		forge.KeyRunnerSecret:  keystore.NewSecret(runnerSecret),
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

// ageKeyWarning is INIT-006's second half: the sentence an operator has to
// have read before they walk away from `init`. It says what docs/security.md
// ("Backups are exactly as private as the age key") and docs/operating.md
// ("The one unrecoverable loss") already say, deliberately in the same
// words — someone who reads this line and then the runbook should hear one
// voice, not two descriptions of the same risk they have to reconcile.
const ageKeyWarning = "the age backup key (" + KeyAgeBackupKey + ") is the one unrecoverable loss: " +
	"snapshots are age-encrypted and you hold the only key, so losing it leaves every backup this " +
	"instance will ever produce permanently unreadable — no recovery path, no reset, nobody to ask. " +
	"Keep a copy somewhere other than the machine you would be recovering from"

// reportKeyMaterial implements INIT-006: one event per piece of key
// material naming where it was stored, then the age backup key's warning,
// all on the job's CORE-002 stream so the CLI and the dashboard report the
// same thing from the same data.
//
// One event per key rather than one event with every key in it: the
// dashboard renders an event as a row of text and the CLI renders it as a
// line, so a detail carrying embedded newlines reads correctly in exactly
// one of the two frontends. The stream is the shared surface, and a list
// belongs in it as list-shaped events.
//
// The location comes from keystore.Target, which asks the configured
// driver and accepts "I don't know" as an answer (keystore/describe.go).
// A driver that keeps key material somewhere farrier cannot name — the
// command driver's operator-supplied command, an out-of-tree exec driver —
// is reported by name alone. Naming the driver an operator configured is
// still the difference between "nine files went somewhere" and "these nine
// pieces went through this driver"; guessing a target would be worse than
// saying less.
//
// Nothing here touches a Secret. The report is built from key names and
// destinations only, so key material cannot reach the event stream even by
// accident (KEY-003).
func reportKeyMaterial(job *events.Job, driverName string, driver keystore.Driver, material map[string]keystore.Secret) {
	job.Started(StepReportKeys, "where each piece of key material was stored")
	for _, name := range keyMaterialOrder {
		if _, ok := material[name]; !ok {
			continue
		}
		job.Emit(StepReportKeys, events.StateSucceeded, fmt.Sprintf("%s → %s", name, describeLocation(driverName, driver, name)))
	}
	if _, ok := material[KeyAgeBackupKey]; ok {
		job.Emit(StepReportKeys, events.StateSucceeded, ageKeyWarning)
	}
}

// describeLocation renders one key's destination: the driver always, and
// its target when the driver reports one.
func describeLocation(driverName string, driver keystore.Driver, keyName string) string {
	if target := keystore.Target(driver, keyName); target != "" {
		return fmt.Sprintf("%s keystore driver, %s", driverName, target)
	}
	return fmt.Sprintf("%s keystore driver (it does not report where key material lands)", driverName)
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
