package restore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
)

func TestRestoreBlobsPutsEveryComponent(t *testing.T) {
	dir := t.TempDir()
	blobsDir := filepath.Join(dir, "blobs")
	if err := os.MkdirAll(filepath.Join(blobsDir, "avatars"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blobsDir, "avatars", "1.png"), []byte("avatar-bytes"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blobsDir, "lfs-1"), []byte("lfs-bytes"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	manifest := &backup.Manifest{Components: []backup.Component{
		{Kind: bundle.StateKindBlobs, Name: "avatars/1.png", Path: "blobs/avatars/1.png"},
		{Kind: bundle.StateKindBlobs, Name: "lfs-1", Path: "blobs/lfs-1"},
		{Kind: bundle.StateKindDatabase, Name: "db.sqlite", Path: "db.sqlite"},
	}}

	target := newFakeBlobTarget()
	job := events.NewJob()
	if err := restoreBlobs(context.Background(), job, dir, manifest, target); err != nil {
		t.Fatalf("restoreBlobs: %v", err)
	}

	if string(target.puts["avatars/1.png"]) != "avatar-bytes" {
		t.Errorf("avatars/1.png = %q, want %q", target.puts["avatars/1.png"], "avatar-bytes")
	}
	if string(target.puts["lfs-1"]) != "lfs-bytes" {
		t.Errorf("lfs-1 = %q, want %q", target.puts["lfs-1"], "lfs-bytes")
	}
	if len(target.puts) != 2 {
		t.Errorf("put %d blob(s), want 2: %v", len(target.puts), target.puts)
	}
}

func TestRestoreBlobsFailsLoudlyOnMissingFile(t *testing.T) {
	dir := t.TempDir()
	manifest := &backup.Manifest{Components: []backup.Component{
		{Kind: bundle.StateKindBlobs, Name: "avatars/1.png", Path: "blobs/avatars/1.png"},
	}}

	target := newFakeBlobTarget()
	job := events.NewJob()
	if err := restoreBlobs(context.Background(), job, dir, manifest, target); err == nil {
		t.Fatal("restoreBlobs: want error for a missing captured blob file, got nil")
	}
}
