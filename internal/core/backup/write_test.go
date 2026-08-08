package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/events"
)

// fakeAdapter is a minimal in-memory blob.Adapter for exercising Write's
// error paths without a real destination.
type fakeAdapter struct {
	objects map[string][]byte
	putErr  error
}

func newFakeAdapter() *fakeAdapter {
	return &fakeAdapter{objects: make(map[string][]byte)}
}

func (f *fakeAdapter) List(ctx context.Context, prefix string) ([]blob.Object, error) {
	var out []blob.Object
	for k, v := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, blob.Object{Key: k, Size: int64(len(v))})
		}
	}
	return out, nil
}

func (f *fakeAdapter) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	v, ok := f.objects[key]
	if !ok {
		return nil, blob.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(v)), nil
}

func (f *fakeAdapter) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if f.putErr != nil {
		return f.putErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.objects[key] = data
	return nil
}

func writeArchiveFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snapshot.age")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestWriteStoresArchiveUnderSnapshotKey(t *testing.T) {
	archivePath := writeArchiveFixture(t, "encrypted-bytes")
	dest := newFakeAdapter()
	job := events.NewJob()
	ts := time.Date(2026, 8, 8, 15, 30, 0, 0, time.UTC)

	key, err := Write(context.Background(), job, dest, archivePath, ts)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if want := SnapshotKey(ts); key != want {
		t.Errorf("key = %q, want %q", key, want)
	}
	if string(dest.objects[key]) != "encrypted-bytes" {
		t.Errorf("stored content = %q, want %q", dest.objects[key], "encrypted-bytes")
	}
}

func TestWriteEmitsStepEvents(t *testing.T) {
	archivePath := writeArchiveFixture(t, "encrypted-bytes")
	dest := newFakeAdapter()
	job := events.NewJob()

	if _, err := Write(context.Background(), job, dest, archivePath, time.Now()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	evs := job.Events()
	if len(evs) != 2 {
		t.Fatalf("events = %+v, want exactly 2 (started, succeeded)", evs)
	}
	if evs[0].Step != StepWrite || evs[0].State != events.StateStarted {
		t.Errorf("first event = %+v, want a started %s event", evs[0], StepWrite)
	}
	if evs[1].Step != StepWrite || evs[1].State != events.StateSucceeded {
		t.Errorf("second event = %+v, want a succeeded %s event", evs[1], StepWrite)
	}
	if job.Done() {
		t.Error("Write must not end job — a backup job composes further steps around it")
	}
}

func TestWriteRejectsMissingInputs(t *testing.T) {
	archivePath := writeArchiveFixture(t, "encrypted-bytes")
	dest := newFakeAdapter()

	cases := []struct {
		name        string
		dest        blob.Adapter
		archivePath string
	}{
		{"missing destination", nil, archivePath},
		{"missing archive path", dest, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := events.NewJob()
			if _, err := Write(context.Background(), job, tc.dest, tc.archivePath, time.Now()); err == nil {
				t.Fatal("Write: want error, got nil")
			}
			evs := job.Events()
			last := evs[len(evs)-1]
			if last.Step != StepWrite || last.State != events.StateFailed {
				t.Errorf("last event = %+v, want a failed %s event", last, StepWrite)
			}
			if job.Done() {
				t.Error("Write must not end job on a step failure — the caller's backup job owns the terminal event")
			}
		})
	}
}

func TestWritePropagatesPutError(t *testing.T) {
	archivePath := writeArchiveFixture(t, "encrypted-bytes")
	dest := newFakeAdapter()
	dest.putErr = errors.New("destination unreachable")
	job := events.NewJob()

	_, err := Write(context.Background(), job, dest, archivePath, time.Now())
	if err == nil {
		t.Fatal("Write: want error, got nil")
	}
	if !errors.Is(err, dest.putErr) {
		t.Errorf("Write: error = %v, want it to wrap %v", err, dest.putErr)
	}
}

func TestWriteRejectsMissingArchiveFile(t *testing.T) {
	dest := newFakeAdapter()
	job := events.NewJob()
	missing := filepath.Join(t.TempDir(), "does-not-exist.age")

	if _, err := Write(context.Background(), job, dest, missing, time.Now()); err == nil {
		t.Fatal("Write: want error for a missing archive file, got nil")
	}
}

func TestSnapshotKeyFormat(t *testing.T) {
	ts := time.Date(2026, 8, 8, 15, 30, 0, 0, time.FixedZone("PST", -8*60*60))
	got := SnapshotKey(ts)
	want := "20260808T233000Z.age"
	if got != want {
		t.Errorf("SnapshotKey = %q, want %q (must normalize to UTC)", got, want)
	}
}
