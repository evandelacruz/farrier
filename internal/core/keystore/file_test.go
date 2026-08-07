package keystore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFileResolveReadsKeyContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret-key"), []byte("s3cr3t\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	r, err := NewFile(dir)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	got, err := r.Resolve(context.Background(), "secret-key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := "s3cr3t"; got.Reveal() != want {
		t.Fatalf("Reveal() = %q, want %q (trailing newline should be trimmed)", got.Reveal(), want)
	}
}

func TestFileResolveMissingKeyErrors(t *testing.T) {
	dir := t.TempDir()
	r, err := NewFile(dir)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if _, err := r.Resolve(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("Resolve: want error for missing key, got nil")
	}
}

func TestFileResolveRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	// A file outside dir that traversal would otherwise reach.
	outside := filepath.Join(filepath.Dir(dir), "outside-secret")
	if err := os.WriteFile(outside, []byte("leak"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	defer os.Remove(outside)

	r, err := NewFile(dir)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if _, err := r.Resolve(context.Background(), "../outside-secret"); err == nil {
		t.Fatal("Resolve: want error for path traversal, got nil")
	}
}

func TestFileResolveRejectsAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	r, err := NewFile(dir)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if _, err := r.Resolve(context.Background(), "/etc/passwd"); err == nil {
		t.Fatal("Resolve: want error for absolute path, got nil")
	}
}

func TestFileResolveEmptyKeyNameErrors(t *testing.T) {
	dir := t.TempDir()
	r, err := NewFile(dir)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if _, err := r.Resolve(context.Background(), ""); err == nil {
		t.Fatal("Resolve: want error for empty key name, got nil")
	}
}

func TestNewFileRejectsEmptyDir(t *testing.T) {
	if _, err := NewFile(""); err == nil {
		t.Fatal("NewFile: want error for empty dir, got nil")
	}
}

func TestFileResolveContextCanceled(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "k"), []byte("v"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	r, err := NewFile(dir)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Resolve(ctx, "k"); err == nil {
		t.Fatal("Resolve: want error for canceled context, got nil")
	}
}
