package backup

import (
	"bytes"
	"context"
	"errors"
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

func TestSnapshotAgeRejectsNilDestination(t *testing.T) {
	if _, _, err := SnapshotAge(context.Background(), nil, "", time.Now()); err == nil {
		t.Fatal("SnapshotAge(nil): want error, got nil")
	}
}

func TestSnapshotAgeRejectsEmptyDestination(t *testing.T) {
	adapter := newFakeAdapter()
	if _, _, err := SnapshotAge(context.Background(), adapter, "", time.Now()); err == nil {
		t.Fatal("SnapshotAge: want error for a destination with no snapshots, got nil")
	}
}

func TestSnapshotAgeRejectsMissingKey(t *testing.T) {
	adapter := newFakeAdapter()
	if err := adapter.Put(context.Background(), "20260101T000000Z.age", bytes.NewReader([]byte("a")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, _, err := SnapshotAge(context.Background(), adapter, "does-not-exist.age", time.Now()); err == nil {
		t.Fatal("SnapshotAge: want error for a key not in the destination, got nil")
	}
}

func TestSnapshotAgeDefaultsToLatest(t *testing.T) {
	// A real destination, not fakeAdapter: SnapshotAge sorts by
	// Object.Modified via LatestSnapshotKey, which fakeAdapter never
	// populates (write_test.go's own doc comment: it exists for Write's
	// error paths, not for Modified-based ordering).
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

	now := time.Now()
	newerModified := now.Add(-3 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, older), now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := os.Chtimes(filepath.Join(dir, newer), newerModified, newerModified); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	key, age, err := SnapshotAge(context.Background(), adapter, "", now)
	if err != nil {
		t.Fatalf("SnapshotAge: %v", err)
	}
	if key != newer {
		t.Errorf("SnapshotAge key = %q, want %q", key, newer)
	}
	if age < 2*time.Hour || age > 4*time.Hour {
		t.Errorf("SnapshotAge age = %v, want ~3h", age)
	}
}

func TestSnapshotAgeResolvesNamedKey(t *testing.T) {
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

	now := time.Now()
	olderModified := now.Add(-240 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, older), olderModified, olderModified); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := os.Chtimes(filepath.Join(dir, newer), now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Naming the older key explicitly must resolve to its own age, not the
	// newest object's — SnapshotAge only defaults to latest when key is
	// empty.
	key, age, err := SnapshotAge(context.Background(), adapter, older, now)
	if err != nil {
		t.Fatalf("SnapshotAge: %v", err)
	}
	if key != older {
		t.Errorf("SnapshotAge key = %q, want %q", key, older)
	}
	if age < 239*time.Hour || age > 241*time.Hour {
		t.Errorf("SnapshotAge age = %v, want ~240h", age)
	}
}

func TestSnapshotAgeClampsFutureModifiedToZero(t *testing.T) {
	dir := t.TempDir()
	adapter, err := OpenDestination(dir)
	if err != nil {
		t.Fatalf("OpenDestination: %v", err)
	}

	key := SnapshotKey(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err := adapter.Put(context.Background(), key, bytes.NewReader([]byte("a")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}

	now := time.Now()
	future := now.Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, key), future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	_, age, err := SnapshotAge(context.Background(), adapter, key, now)
	if err != nil {
		t.Fatalf("SnapshotAge: %v", err)
	}
	if age != 0 {
		t.Errorf("SnapshotAge age = %v, want 0 (clamped) for a destination clock reading ahead of now", age)
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

func TestResolveOptionalDestinationEmptyIsNil(t *testing.T) {
	dest, err := ResolveOptionalDestination("")
	if err != nil {
		t.Fatalf("ResolveOptionalDestination(\"\"): %v", err)
	}
	if dest != nil {
		t.Fatalf("ResolveOptionalDestination(\"\") = %v, want nil", dest)
	}

	dest, err = ResolveOptionalDestination("   ")
	if err != nil {
		t.Fatalf("ResolveOptionalDestination(\"   \"): %v", err)
	}
	if dest != nil {
		t.Fatalf("ResolveOptionalDestination(\"   \") = %v, want nil", dest)
	}
}

func TestResolveOptionalDestinationResolvesFilesystemPath(t *testing.T) {
	dir := t.TempDir() + "/backups"
	dest, err := ResolveOptionalDestination(dir)
	if err != nil {
		t.Fatalf("ResolveOptionalDestination(%q): %v", dir, err)
	}
	if _, ok := dest.(*blob.LocalAdapter); !ok {
		t.Fatalf("ResolveOptionalDestination(%q) = %T, want *blob.LocalAdapter", dir, dest)
	}
}

func TestResolveOptionalDestinationPropagatesError(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	if _, err := ResolveOptionalDestination("s3://my-bucket?endpoint=localhost:9000"); err == nil {
		t.Fatal("ResolveOptionalDestination: want error when AWS credentials are unset, got nil")
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

func TestHistoryRejectsNilDestination(t *testing.T) {
	if _, err := History(context.Background(), nil, time.Now()); err == nil {
		t.Fatal("History(nil): want error, got nil")
	}
}

// An empty destination is a valid, empty history — an operator who has
// configured a destination but not yet run `backup` is not in an error
// state, which is where History deliberately differs from LatestSnapshotKey.
func TestHistoryAllowsEmptyDestination(t *testing.T) {
	snapshots, err := History(context.Background(), newFakeAdapter(), time.Now())
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("History = %+v, want no snapshots", snapshots)
	}
}

func TestHistoryReturnsNewestFirstWithAges(t *testing.T) {
	// A real destination, not fakeAdapter: History sorts by
	// Object.Modified, which fakeAdapter never populates.
	dir := t.TempDir()
	adapter, err := OpenDestination(dir)
	if err != nil {
		t.Fatalf("OpenDestination: %v", err)
	}

	older := SnapshotKey(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	newer := SnapshotKey(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err := adapter.Put(context.Background(), older, bytes.NewReader([]byte("aa")), 2); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := adapter.Put(context.Background(), newer, bytes.NewReader([]byte("bbb")), 3); err != nil {
		t.Fatalf("Put: %v", err)
	}

	now := time.Now()
	olderModified := now.Add(-48 * time.Hour)
	newerModified := now.Add(-3 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, older), olderModified, olderModified); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := os.Chtimes(filepath.Join(dir, newer), newerModified, newerModified); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	snapshots, err := History(context.Background(), adapter, now)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("History = %+v, want 2 snapshots", snapshots)
	}
	if snapshots[0].Key != newer || snapshots[1].Key != older {
		t.Errorf("History keys = %q, %q; want %q first", snapshots[0].Key, snapshots[1].Key, newer)
	}
	if snapshots[0].SizeBytes != 3 {
		t.Errorf("newest SizeBytes = %d, want 3", snapshots[0].SizeBytes)
	}
	if snapshots[0].Age < 2*time.Hour || snapshots[0].Age > 4*time.Hour {
		t.Errorf("newest Age = %v, want ~3h", snapshots[0].Age)
	}
	if snapshots[1].Age < 47*time.Hour || snapshots[1].Age > 49*time.Hour {
		t.Errorf("oldest Age = %v, want ~48h", snapshots[1].Age)
	}
}

// A destination whose clock reads ahead of the control plane must not yield
// a negative age — the same clamp SnapshotAge applies.
func TestHistoryClampsFutureModifiedToZeroAge(t *testing.T) {
	dir := t.TempDir()
	adapter, err := OpenDestination(dir)
	if err != nil {
		t.Fatalf("OpenDestination: %v", err)
	}

	key := SnapshotKey(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err := adapter.Put(context.Background(), key, bytes.NewReader([]byte("a")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	now := time.Now()
	ahead := now.Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, key), ahead, ahead); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	snapshots, err := History(context.Background(), adapter, now)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].Age != 0 {
		t.Fatalf("History = %+v, want one snapshot with zero age", snapshots)
	}
}

// History reports a listing failure rather than an empty history: an
// unreachable destination and a destination with no snapshots must never
// look the same to an operator.
func TestHistoryReportsListFailure(t *testing.T) {
	_, err := History(context.Background(), listErrAdapter{}, time.Now())
	if err == nil {
		t.Fatal("History: want error when the destination cannot be listed, got nil")
	}
}

type listErrAdapter struct{}

func (listErrAdapter) List(context.Context, string) ([]blob.Object, error) {
	return nil, errors.New("destination unreachable")
}
func (listErrAdapter) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, blob.ErrNotFound
}
func (listErrAdapter) Put(context.Context, string, io.Reader, int64) error { return nil }
