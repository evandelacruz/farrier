package keystore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// TestNewFileDriverRejectsRelativePath proves XCUT-001's constraint at the
// keystore file driver: config.path must be absolute. A relative path
// bakes into the bundle manifest at init and would silently re-resolve
// against whatever directory a later command happens to run from — a
// different one on another machine, or even the same machine from a
// different shell session — instead of failing loudly or staying put.
func TestNewFileDriverRejectsRelativePath(t *testing.T) {
	_, err := New("file", map[string]any{"path": "relative/keys"})
	if err == nil {
		t.Fatal("New: want error for relative config.path, got nil")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("New: err = %v, want it to mention the path must be absolute", err)
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

func TestFileDriverStoreRefusesToOverwriteExistingKey(t *testing.T) {
	dir := t.TempDir()
	d := FileDriver{Path: dir}
	if err := d.Store(context.Background(), "k", NewSecret("original")); err != nil {
		t.Fatalf("Store: %v", err)
	}

	err := d.Store(context.Background(), "k", NewSecret("replacement"))
	if err == nil {
		t.Fatal("Store: want error overwriting an existing key, got nil")
	}

	got, resolveErr := d.Resolve(context.Background(), "k")
	if resolveErr != nil {
		t.Fatalf("Resolve: %v", resolveErr)
	}
	if got.Reveal() != "original" {
		t.Errorf("Reveal() = %q, want original value preserved", got.Reveal())
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
