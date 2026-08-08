package keystore

import (
	"context"
	"testing"
)

func TestRotatesDefaultsToFalse(t *testing.T) {
	if Rotates("some_key_nobody_declared") {
		t.Error("Rotates: want false for a key absent from the registry")
	}
}

func TestRotatesTrueForTLSKeys(t *testing.T) {
	if !Rotates(KeyTLSCertificate) {
		t.Errorf("Rotates(%q) = false, want true", KeyTLSCertificate)
	}
	if !Rotates(KeyTLSPrivateKey) {
		t.Errorf("Rotates(%q) = false, want true", KeyTLSPrivateKey)
	}
}

// TestNewFileDriverRefusesToOverwriteNonRotatingKey exercises the
// invariant the rotation guard exists for: once a non-rotating key has
// content, New's wrapped driver refuses to replace it — the same
// protection that used to live inside FileDriver.Store itself.
func TestNewFileDriverRefusesToOverwriteNonRotatingKey(t *testing.T) {
	dir := t.TempDir()
	d, err := New("file", map[string]any{"path": dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writer := d.(Writer)

	if err := writer.Store(context.Background(), "forgejo_secret_key", NewSecret("original")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := writer.Store(context.Background(), "forgejo_secret_key", NewSecret("replacement")); err == nil {
		t.Fatal("Store: want error overwriting a non-rotating key, got nil")
	}

	got, err := d.Resolve(context.Background(), "forgejo_secret_key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Reveal() != "original" {
		t.Errorf("Reveal() = %q, want original value preserved", got.Reveal())
	}
}

// TestNewFileDriverSecondInitFailsOnAlreadyPopulatedKeystore is the safety
// property the original FileDriver.Store guard existed for, proved to
// still hold now that enforcement sits above the driver instead of inside
// it: running init's own Store sequence twice against the same keystore
// target must fail on the first key rather than silently rotating a live
// instance's identity.
func TestNewFileDriverSecondInitFailsOnAlreadyPopulatedKeystore(t *testing.T) {
	dir := t.TempDir()
	d, err := New("file", map[string]any{"path": dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writer := d.(Writer)

	initKeys := []string{"forgejo_secret_key", "forgejo_internal_token", "forgejo_lfs_jwt_secret", "ssh_host_key"}
	for _, key := range initKeys {
		if err := writer.Store(context.Background(), key, NewSecret("value-for-"+key)); err != nil {
			t.Fatalf("first init: Store(%q): %v", key, err)
		}
	}

	// A second init run against the same populated keystore target must
	// fail on the very first key it tries to store, not silently succeed.
	if err := writer.Store(context.Background(), initKeys[0], NewSecret("rotated-by-mistake")); err == nil {
		t.Fatalf("second init: Store(%q): want error, got nil — identity would have silently rotated", initKeys[0])
	}

	got, err := d.Resolve(context.Background(), initKeys[0])
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Reveal() != "value-for-"+initKeys[0] {
		t.Errorf("Reveal() = %q, want the first init's value preserved", got.Reveal())
	}
}

// TestNewFileDriverAllowsOverwritingRotatingKey exercises ACME-002's
// renewal write path: unlike every other piece of key material, the TLS
// certificate and its private key are declared rotating, so New's wrapped
// driver allows a renewed certificate to replace the one init persisted.
func TestNewFileDriverAllowsOverwritingRotatingKey(t *testing.T) {
	dir := t.TempDir()
	d, err := New("file", map[string]any{"path": dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writer := d.(Writer)

	if err := writer.Store(context.Background(), KeyTLSCertificate, NewSecret("original-cert")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := writer.Store(context.Background(), KeyTLSCertificate, NewSecret("renewed-cert")); err != nil {
		t.Fatalf("Store renewal: %v", err)
	}

	got, err := d.Resolve(context.Background(), KeyTLSCertificate)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Reveal() != "renewed-cert" {
		t.Errorf("Reveal() = %q, want the renewed value", got.Reveal())
	}
}

// TestNewWrapsWriterCapableDrivers confirms a driver built through New
// still satisfies Writer via a type assertion, the pattern
// initialize.Run uses to discover write support — the guard wraps FileDriver
// transparently rather than hiding its Writer-ness.
func TestNewWrapsWriterCapableDrivers(t *testing.T) {
	d, err := New("file", map[string]any{"path": t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := d.(Writer); !ok {
		t.Fatal("New(\"file\", ...) does not implement Writer")
	}
}

// TestNewDoesNotWrapReadOnlyDrivers confirms CommandDriver, which has no
// Store method at all, passes through New unwrapped and still fails a
// Writer type assertion the way callers rely on (KEY-002).
func TestNewDoesNotWrapReadOnlyDrivers(t *testing.T) {
	d, err := New("command", map[string]any{"command": "true"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := d.(Writer); ok {
		t.Fatal("New(\"command\", ...) implements Writer, want it to stay read-only")
	}
}
