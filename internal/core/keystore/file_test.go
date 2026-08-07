package keystore

import (
	"context"
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
