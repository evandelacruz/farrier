package state

import (
	"bytes"
	"context"
	"io"
	"sort"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/blob"
)

// TestLocalAdapterSatisfiesBlobExporter exercises a blob.LocalAdapter
// through the BlobExporter interface — not through blob.Adapter — proving
// STATE-003's claim that blobs are exportable through the blob adapter
// interface itself, with no adapting code in between.
func TestLocalAdapterSatisfiesBlobExporter(t *testing.T) {
	adapter, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()

	seed := map[string][]byte{
		"lfs/objects/aa/bb/sha256-1": []byte("lfs object contents"),
		"lfs/objects/cc/dd/sha256-2": []byte("another lfs object"),
		"avatars/1.png":              []byte("fake avatar bytes"),
	}
	for key, content := range seed {
		if err := adapter.Put(ctx, key, bytes.NewReader(content), int64(len(content))); err != nil {
			t.Fatalf("seed Put(%s): %v", key, err)
		}
	}

	var exporter BlobExporter = adapter

	objects, err := exporter.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != len(seed) {
		t.Fatalf("List returned %d objects, want %d", len(objects), len(seed))
	}

	var gotKeys []string
	for _, o := range objects {
		gotKeys = append(gotKeys, o.Key)
		want, ok := seed[o.Key]
		if !ok {
			t.Fatalf("List returned unexpected key %q", o.Key)
		}
		if o.Size != int64(len(want)) {
			t.Fatalf("List(%s).Size = %d, want %d", o.Key, o.Size, len(want))
		}
	}
	sort.Strings(gotKeys)
	var wantKeys []string
	for k := range seed {
		wantKeys = append(wantKeys, k)
	}
	sort.Strings(wantKeys)
	for i := range wantKeys {
		if gotKeys[i] != wantKeys[i] {
			t.Fatalf("List keys = %v, want %v", gotKeys, wantKeys)
		}
	}

	lfsOnly, err := exporter.List(ctx, "lfs/")
	if err != nil {
		t.Fatalf("List(lfs/): %v", err)
	}
	if len(lfsOnly) != 2 {
		t.Fatalf("List(lfs/) returned %d objects, want 2", len(lfsOnly))
	}

	for key, want := range seed {
		rc, err := exporter.Get(ctx, key)
		if err != nil {
			t.Fatalf("Get(%s): %v", key, err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Get(%s) = %q, want %q", key, got, want)
		}
	}
}

func TestBlobExporterGetMissingKeyReturnsErrNotFound(t *testing.T) {
	adapter, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	var exporter BlobExporter = adapter
	if _, err := exporter.Get(context.Background(), "does/not/exist"); err == nil {
		t.Fatal("Get(missing): want error, got nil")
	}
}
