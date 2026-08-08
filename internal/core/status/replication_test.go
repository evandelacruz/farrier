package status

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/blob"
)

func TestReplicationLagNilDestIsUnmeasured(t *testing.T) {
	lag, err := ReplicationLag(context.Background(), nil, time.Now())
	if err != nil {
		t.Fatalf("ReplicationLag: %v", err)
	}
	if lag.State != LagUnmeasured {
		t.Fatalf("State = %q, want %q", lag.State, LagUnmeasured)
	}
}

func TestReplicationLagEmptyDestIsNoBackups(t *testing.T) {
	a, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	lag, err := ReplicationLag(context.Background(), a, time.Now())
	if err != nil {
		t.Fatalf("ReplicationLag: %v", err)
	}
	if lag.State != LagNoBackups {
		t.Fatalf("State = %q, want %q", lag.State, LagNoBackups)
	}
}

func TestReplicationLagMeasuresFromNewestObject(t *testing.T) {
	a, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()

	if err := a.Put(ctx, "snapshot-1", bytes.NewReader([]byte("older")), 5); err != nil {
		t.Fatalf("Put snapshot-1: %v", err)
	}
	if err := a.Put(ctx, "snapshot-2", bytes.NewReader([]byte("newer")), 5); err != nil {
		t.Fatalf("Put snapshot-2: %v", err)
	}

	objects, err := a.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var newest time.Time
	for _, o := range objects {
		if o.Modified.After(newest) {
			newest = o.Modified
		}
	}
	if newest.IsZero() {
		t.Fatal("local adapter did not populate Modified")
	}

	now := newest.Add(90 * time.Minute)
	lag, err := ReplicationLag(ctx, a, now)
	if err != nil {
		t.Fatalf("ReplicationLag: %v", err)
	}
	if lag.State != LagMeasured {
		t.Fatalf("State = %q, want %q", lag.State, LagMeasured)
	}
	if !lag.LastBackup.Equal(newest) {
		t.Fatalf("LastBackup = %v, want %v", lag.LastBackup, newest)
	}
	if lag.Age != 90*time.Minute {
		t.Fatalf("Age = %v, want %v", lag.Age, 90*time.Minute)
	}
}

// fakeExporter is a state.BlobExporter that returns objects with whatever
// Modified time a test wants, including the zero value — a stand-in for a
// third-party exec adapter whose "list" response predates the "modified"
// field.
type fakeExporter struct {
	objects []blob.Object
	err     error
}

func (f *fakeExporter) List(ctx context.Context, prefix string) ([]blob.Object, error) {
	return f.objects, f.err
}

func (f *fakeExporter) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return nil, errors.New("fakeExporter: Get not implemented")
}

func TestReplicationLagUnknownModifiedTimesAreUnmeasured(t *testing.T) {
	f := &fakeExporter{objects: []blob.Object{{Key: "a", Size: 1}, {Key: "b", Size: 2}}}

	lag, err := ReplicationLag(context.Background(), f, time.Now())
	if err != nil {
		t.Fatalf("ReplicationLag: %v", err)
	}
	if lag.State != LagUnmeasured {
		t.Fatalf("State = %q, want %q", lag.State, LagUnmeasured)
	}
}

func TestReplicationLagPropagatesListError(t *testing.T) {
	f := &fakeExporter{err: errors.New("boom")}

	_, err := ReplicationLag(context.Background(), f, time.Now())
	if err == nil {
		t.Fatal("ReplicationLag: want error, got nil")
	}
}
