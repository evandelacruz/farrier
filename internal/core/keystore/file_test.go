package keystore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileDriverResolve(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "forgejo_secret_key"), []byte("supersecret"), 0o600); err != nil {
		t.Fatal(err)
	}

	d := FileDriver{Path: dir}
	got, err := d.Resolve(context.Background(), "forgejo_secret_key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Reveal() != "supersecret" {
		t.Fatalf("Reveal() = %q, want %q", got.Reveal(), "supersecret")
	}
}

func TestFileDriverResolveBinaryContentRoundTrips(t *testing.T) {
	dir := t.TempDir()
	want := []byte{0x00, 0x01, 0xff, 0xfe, '\n', 0x00}
	if err := os.WriteFile(filepath.Join(dir, "age_backup_key"), want, 0o600); err != nil {
		t.Fatal(err)
	}

	d := FileDriver{Path: dir}
	got, err := d.Resolve(context.Background(), "age_backup_key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Reveal() != string(want) {
		t.Fatalf("Reveal() = %v, want %v", []byte(got.Reveal()), want)
	}
}

func TestFileDriverResolveMissingKeyErrors(t *testing.T) {
	d := FileDriver{Path: t.TempDir()}
	if _, err := d.Resolve(context.Background(), "does_not_exist"); err == nil {
		t.Fatal("Resolve: want error for missing file, got nil")
	}
}

// TestFileDriverResolveMissingKeyWrapsErrNotFound confirms Resolve reports
// a positive "not found" the way the Driver interface (keystore.go) and
// guardedDriver.Store (guard.go) require: a missing file must be
// distinguishable, via errors.Is, from any other resolve failure.
func TestFileDriverResolveMissingKeyWrapsErrNotFound(t *testing.T) {
	d := FileDriver{Path: t.TempDir()}
	_, err := d.Resolve(context.Background(), "does_not_exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve: err = %v, want it to wrap ErrNotFound", err)
	}
}

// TestFileDriverResolveOtherFailureDoesNotWrapErrNotFound is the other
// side of the same contract: a failure that is not "file does not exist"
// (here, keyName resolving to a directory rather than a file) must not be
// mistaken for ErrNotFound, or guardedDriver.Store would treat an
// indeterminate failure as "safe to overwrite."
func TestFileDriverResolveOtherFailureDoesNotWrapErrNotFound(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "not_a_file"), 0o700); err != nil {
		t.Fatal(err)
	}

	d := FileDriver{Path: dir}
	_, err := d.Resolve(context.Background(), "not_a_file")
	if err == nil {
		t.Fatal("Resolve: want error reading a directory as a key, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve: err = %v, wrongly wraps ErrNotFound for a non-not-found failure", err)
	}
}

func TestFileDriverResolveEmptyFileErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty_key"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	d := FileDriver{Path: dir}
	if _, err := d.Resolve(context.Background(), "empty_key"); err == nil {
		t.Fatal("Resolve: want error for empty file, got nil")
	}
}

func TestFileDriverResolveRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}

	d := FileDriver{Path: dir}
	rel, err := filepath.Rel(dir, filepath.Join(outside, "secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Resolve(context.Background(), rel); err == nil {
		t.Fatal("Resolve: want error for key name escaping configured path, got nil")
	}
}

func TestFileDriverResolveEmptyKeyNameErrors(t *testing.T) {
	d := FileDriver{Path: t.TempDir()}
	if _, err := d.Resolve(context.Background(), ""); err == nil {
		t.Fatal("Resolve: want error for empty key name, got nil")
	}
}

func TestFileDriverResolveCanceledContextErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "k"), []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := FileDriver{Path: dir}
	if _, err := d.Resolve(ctx, "k"); err == nil {
		t.Fatal("Resolve: want error for canceled context, got nil")
	}
}

func TestNewFileDriverRequiresPath(t *testing.T) {
	if _, err := New("file", map[string]any{}); err == nil {
		t.Fatal("New: want error for missing config.path, got nil")
	}
}

func TestFileDriverStoreThenResolveRoundTrips(t *testing.T) {
	dir := t.TempDir()
	d := FileDriver{Path: dir}

	if err := d.Store(context.Background(), "forgejo_secret_key", NewSecret("supersecret")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := d.Resolve(context.Background(), "forgejo_secret_key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Reveal() != "supersecret" {
		t.Fatalf("Reveal() = %q, want %q", got.Reveal(), "supersecret")
	}
	info, err := os.Stat(filepath.Join(dir, "forgejo_secret_key"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestFileDriverStoreCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "keys")
	d := FileDriver{Path: dir}

	if err := d.Store(context.Background(), "k", NewSecret("v")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if got, err := d.Resolve(context.Background(), "k"); err != nil || got.Reveal() != "v" {
		t.Fatalf("Resolve after Store = (%q, %v), want (\"v\", nil)", got.Reveal(), err)
	}
}

// TestFileDriverStoreOverwritesWithoutGuard documents that the bare driver
// has no overwrite guard of its own: that check moved up into New's
// guardedDriver (guard_test.go), so every driver gets it once instead of
// each driver implementing it separately.
func TestFileDriverStoreOverwritesWithoutGuard(t *testing.T) {
	dir := t.TempDir()
	d := FileDriver{Path: dir}
	if err := d.Store(context.Background(), "k", NewSecret("original")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := d.Store(context.Background(), "k", NewSecret("replacement")); err != nil {
		t.Fatalf("Store: %v", err)
	}

	got, resolveErr := d.Resolve(context.Background(), "k")
	if resolveErr != nil {
		t.Fatalf("Resolve: %v", resolveErr)
	}
	if got.Reveal() != "replacement" {
		t.Errorf("Reveal() = %q, want the replacement value", got.Reveal())
	}
}

func TestFileDriverStoreRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()

	d := FileDriver{Path: dir}
	rel, err := filepath.Rel(dir, filepath.Join(outside, "secret"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Store(context.Background(), rel, NewSecret("nope")); err == nil {
		t.Fatal("Store: want error for key name escaping configured path, got nil")
	}
}

func TestFileDriverStoreEmptyKeyNameErrors(t *testing.T) {
	d := FileDriver{Path: t.TempDir()}
	if err := d.Store(context.Background(), "", NewSecret("v")); err == nil {
		t.Fatal("Store: want error for empty key name, got nil")
	}
}

func TestFileDriverStoreCanceledContextErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := FileDriver{Path: t.TempDir()}
	if err := d.Store(ctx, "k", NewSecret("v")); err == nil {
		t.Fatal("Store: want error for canceled context, got nil")
	}
}

func TestNewFileDriverResolvesThroughFactory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "k"), []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := New("file", map[string]any{"path": dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := d.Resolve(context.Background(), "k")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Reveal() != "v" {
		t.Fatalf("Reveal() = %q, want %q", got.Reveal(), "v")
	}
}
