package orchestrate

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

// newTestKeyPair generates a fresh ed25519 key pair for test use, returning
// both the raw private key (for handing to an in-memory SSH agent) and the
// derived SSH signer (for a fake server's public-key check).
func newTestKeyPair(t *testing.T) (ed25519.PrivateKey, ssh.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer from key: %v", err)
	}
	return priv, signer
}

// newTestSigner generates a fresh ed25519 SSH signer for use as a test host
// key.
func newTestSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, signer := newTestKeyPair(t)
	return signer
}

// writeTestKeyFile generates an ed25519 key, writes its PKCS8 PEM encoding
// to a file under t.TempDir(), and returns both the file path and the
// resulting signer (so a test can assert the server saw the matching
// public key).
func writeTestKeyFile(t *testing.T) (path string, signer ssh.Signer) {
	t.Helper()
	priv, signer := newTestKeyPair(t)

	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: der}

	path = filepath.Join(t.TempDir(), "id_test")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path, signer
}
