package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestLocalPutGetRoundTrip(t *testing.T) {
	a, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()

	content := []byte("hello, blob")
	if err := a.Put(ctx, "objects/a.bin", bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := a.Get(ctx, "objects/a.bin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("got %q, want %q", got, content)
	}
}

func TestLocalListPopulatesModified(t *testing.T) {
	a, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()
	before := time.Now().Add(-time.Second)

	if err := a.Put(ctx, "k", bytes.NewReader([]byte("v")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}

	objects, err := a.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("List returned %d objects, want 1", len(objects))
	}
	if objects[0].Modified.Before(before) {
		t.Fatalf("Modified = %v, want at or after %v", objects[0].Modified, before)
	}
}

func TestLocalGetMissingKeyReturnsErrNotFound(t *testing.T) {
	a, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	_, err = a.Get(context.Background(), "does/not/exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

func TestLocalPutOverwritesWhole(t *testing.T) {
	a, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()

	if err := a.Put(ctx, "k", bytes.NewReader([]byte("first-version-longer")), 20); err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	if err := a.Put(ctx, "k", bytes.NewReader([]byte("v2")), 2); err != nil {
		t.Fatalf("Put 2: %v", err)
	}

	rc, err := a.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "v2" {
		t.Fatalf("got %q, want %q (overwrite should not leave trailing bytes)", got, "v2")
	}
}

func TestLocalList(t *testing.T) {
	a, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()

	keys := []string{"lfs/a", "lfs/b", "artifacts/c"}
	for _, k := range keys {
		if err := a.Put(ctx, k, bytes.NewReader([]byte(k)), int64(len(k))); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}

	objects, err := a.List(ctx, "lfs/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := make([]string, len(objects))
	for i, o := range objects {
		got[i] = o.Key
	}
	sort.Strings(got)
	want := []string{"lfs/a", "lfs/b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("List(%q) = %v, want %v", "lfs/", got, want)
	}
}

func TestLocalListEmptyPrefixListsEverything(t *testing.T) {
	a, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()
	for _, k := range []string{"a", "b/c"} {
		if err := a.Put(ctx, k, bytes.NewReader([]byte(k)), int64(len(k))); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}

	objects, err := a.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 2 {
		t.Fatalf("List(\"\") returned %d objects, want 2", len(objects))
	}
}

func TestLocalRejectsPathTraversal(t *testing.T) {
	a, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()

	if err := a.Put(ctx, "../escape", bytes.NewReader([]byte("x")), 1); err == nil {
		t.Fatal("Put(\"../escape\"): want error, got nil")
	}
	if _, err := a.Get(ctx, "../../etc/passwd"); err == nil {
		t.Fatal("Get(\"../../etc/passwd\"): want error, got nil")
	}
}

func TestLocalPutStreamsWithoutLeavingTempFiles(t *testing.T) {
	root := t.TempDir()
	a, err := NewLocal(root)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	content := []byte("streamed content")
	if err := a.Put(context.Background(), "nested/dir/file.bin", bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var leftoverTemp bool
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(path) == ".tmp" {
			leftoverTemp = true
		}
		return nil
	})
	if leftoverTemp {
		t.Fatal("Put left a .tmp file behind after a successful write")
	}
}
