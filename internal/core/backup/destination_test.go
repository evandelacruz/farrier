package backup

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/events"
)

func TestOpenDestinationRejectsEmpty(t *testing.T) {
	if _, err := OpenDestination(""); err == nil {
		t.Fatal("OpenDestination(\"\"): want error, got nil")
	}
	if _, err := OpenDestination("   "); err == nil {
		t.Fatal("OpenDestination(\"   \"): want error, got nil")
	}
}

func TestOpenDestinationFilesystemPath(t *testing.T) {
	dir := t.TempDir() + "/backups"
	adapter, err := OpenDestination(dir)
	if err != nil {
		t.Fatalf("OpenDestination(%q): %v", dir, err)
	}
	if _, ok := adapter.(*blob.LocalAdapter); !ok {
		t.Fatalf("OpenDestination(%q) = %T, want *blob.LocalAdapter", dir, adapter)
	}

	// Confirm the returned adapter actually works end to end.
	if err := adapter.Put(context.Background(), "k", bytes.NewReader([]byte("v")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := adapter.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "v" {
		t.Errorf("Get = %q, want %q", got, "v")
	}
}

func TestOpenDestinationS3RequiresBucket(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	if _, err := OpenDestination("s3:///prefix?endpoint=localhost:9000"); err == nil {
		t.Fatal("OpenDestination: want error for a missing bucket, got nil")
	}
}

func TestOpenDestinationS3RequiresEndpoint(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	if _, err := OpenDestination("s3://my-bucket"); err == nil {
		t.Fatal("OpenDestination: want error for a missing endpoint, got nil")
	}
}

func TestOpenDestinationS3RequiresCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	if _, err := OpenDestination("s3://my-bucket?endpoint=localhost:9000"); err == nil {
		t.Fatal("OpenDestination: want error when AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY are unset, got nil")
	}
}

func TestOpenDestinationS3RejectsInvalidBoolQuery(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	cases := []string{
		"s3://my-bucket?endpoint=localhost:9000&ssl=maybe",
		"s3://my-bucket?endpoint=localhost:9000&pathStyle=maybe",
	}
	for _, uri := range cases {
		if _, err := OpenDestination(uri); err == nil {
			t.Errorf("OpenDestination(%q): want error, got nil", uri)
		}
	}
}

func TestOpenDestinationS3BuildsAdapter(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	adapter, err := OpenDestination("s3://my-bucket?endpoint=localhost:9000&region=us-west-2&pathStyle=true&ssl=false")
	if err != nil {
		t.Fatalf("OpenDestination: %v", err)
	}
	if _, ok := adapter.(*blob.S3Adapter); !ok {
		t.Fatalf("OpenDestination = %T, want *blob.S3Adapter", adapter)
	}
}

func TestOpenDestinationS3WithPrefixReturnsPrefixedAdapter(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	adapter, err := OpenDestination("s3://my-bucket/env/prod?endpoint=localhost:9000")
	if err != nil {
		t.Fatalf("OpenDestination: %v", err)
	}
	if _, ok := adapter.(*prefixedAdapter); !ok {
		t.Fatalf("OpenDestination with a path segment = %T, want *prefixedAdapter", adapter)
	}
}

func TestPrefixedAdapterScopesKeys(t *testing.T) {
	inner := newFakeAdapter()
	a := &prefixedAdapter{inner: inner, prefix: "env/prod/"}

	if err := a.Put(context.Background(), "snap.age", bytes.NewReader([]byte("data")), 4); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok := inner.objects["env/prod/snap.age"]; !ok {
		t.Fatalf("inner objects = %v, want key %q present", inner.objects, "env/prod/snap.age")
	}

	rc, err := a.Get(context.Background(), "snap.age")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "data" {
		t.Errorf("Get = %q, want %q", got, "data")
	}

	objects, err := a.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 1 || objects[0].Key != "snap.age" {
		t.Fatalf("List = %+v, want one object keyed %q (prefix stripped)", objects, "snap.age")
	}
}

func TestLatestSnapshotKeyRejectsNilDestination(t *testing.T) {
	if _, err := LatestSnapshotKey(context.Background(), nil); err == nil {
		t.Fatal("LatestSnapshotKey(nil): want error, got nil")
	}
}

func TestLatestSnapshotKeyRejectsEmptyDestination(t *testing.T) {
	adapter := newFakeAdapter()
	if _, err := LatestSnapshotKey(context.Background(), adapter); err == nil {
		t.Fatal("LatestSnapshotKey: want error for a destination with no snapshots, got nil")
	}
}

func TestLatestSnapshotKeyReturnsNewest(t *testing.T) {
	// A real destination, not fakeAdapter: LatestSnapshotKey sorts by
	// Object.Modified, which fakeAdapter never populates (write_test.go's
	// own doc comment: it exists for Write's error paths, not for
	// Modified-based ordering).
	dir := t.TempDir()
	adapter, err := OpenDestination(dir)
	if err != nil {
		t.Fatalf("OpenDestination: %v", err)
	}

	older := SnapshotKey(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	newer := SnapshotKey(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err := adapter.Put(context.Background(), older, bytes.NewReader([]byte("a")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := adapter.Put(context.Background(), newer, bytes.NewReader([]byte("b")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Both Puts land at whatever real wall-clock instant this test runs
	// at, which can round to the same mtime on some filesystems — set
	// them explicitly far apart so the assertion is about LatestSnapshotKey
	// picking the newer Modified time, not filesystem timestamp
	// resolution.
	now := time.Now()
	if err := os.Chtimes(filepath.Join(dir, older), now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := os.Chtimes(filepath.Join(dir, newer), now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	got, err := LatestSnapshotKey(context.Background(), adapter)
	if err != nil {
		t.Fatalf("LatestSnapshotKey: %v", err)
	}
	if got != newer {
		t.Errorf("LatestSnapshotKey = %q, want %q", got, newer)
	}
}

func TestFetchDownloadsObject(t *testing.T) {
	adapter := newFakeAdapter()
	if err := adapter.Put(context.Background(), "snap.age", bytes.NewReader([]byte("snapshot-bytes")), -1); err != nil {
		t.Fatalf("Put: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "fetched.age")
	if err := Fetch(context.Background(), adapter, "snap.age", dest); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read fetched file: %v", err)
	}
	if string(got) != "snapshot-bytes" {
		t.Errorf("fetched content = %q, want %q", got, "snapshot-bytes")
	}
}

func TestFetchRejectsMissingKey(t *testing.T) {
	adapter := newFakeAdapter()
	if err := Fetch(context.Background(), adapter, "does-not-exist", filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("Fetch: want error for a missing key, got nil")
	}
}

func TestOpenDestinationThenWriteRoundTrips(t *testing.T) {
	dir := t.TempDir()
	adapter, err := OpenDestination(dir)
	if err != nil {
		t.Fatalf("OpenDestination: %v", err)
	}

	archivePath := writeArchiveFixture(t, "encrypted-bytes")
	job := events.NewJob()
	ts := time.Now()

	key, err := Write(context.Background(), job, adapter, archivePath, ts)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	objects, err := adapter.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 1 || objects[0].Key != key {
		t.Fatalf("List = %+v, want one object keyed %q", objects, key)
	}
}
