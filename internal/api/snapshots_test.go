package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/blob"
)

func doSnapshots(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/snapshots?"+query, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandleSnapshotsRequiresDestination(t *testing.T) {
	s := newTestServer()
	for _, query := range []string{"", "to=", "to=%20%20"} {
		rec := doSnapshots(t, s, query)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: status = %d, want %d", query, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestHandleSnapshotsRejectsUnopenableDestination(t *testing.T) {
	s := newTestServer()
	s.openDestination = func(uri string) (blob.Adapter, error) {
		return nil, errors.New("endpoint query parameter is required")
	}
	rec := doSnapshots(t, s, "to="+url.QueryEscape("s3://bucket"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if got := decodeError(t, rec); got == "" {
		t.Fatal("error body is empty, want the destination's own message")
	}
}

// A destination that cannot be listed is a gateway failure, not an empty
// history: "unreachable" and "no snapshots yet" must never look alike.
func TestHandleSnapshotsReportsListFailure(t *testing.T) {
	s := newTestServer()
	s.openDestination = func(uri string) (blob.Adapter, error) { return nil, nil }
	s.snapshotHistory = func(context.Context, blob.Adapter, time.Time) ([]backup.Snapshot, error) {
		return nil, errors.New("destination unreachable")
	}
	rec := doSnapshots(t, s, "to=/backups")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestHandleSnapshotsReturnsHistory(t *testing.T) {
	modified := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	var openedURI string

	s := newTestServer()
	s.openDestination = func(uri string) (blob.Adapter, error) {
		openedURI = uri
		return nil, nil
	}
	s.snapshotHistory = func(context.Context, blob.Adapter, time.Time) ([]backup.Snapshot, error) {
		return []backup.Snapshot{
			{Key: "20260601T120000Z.age", SizeBytes: 4096, Modified: modified, Age: 90 * time.Minute},
			{Key: "20260501T120000Z.age", SizeBytes: 2048, Modified: modified.AddDate(0, -1, 0), Age: 745 * time.Hour},
		}, nil
	}

	rec := doSnapshots(t, s, "to="+url.QueryEscape("/var/backups"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if openedURI != "/var/backups" {
		t.Errorf("opened destination %q, want %q", openedURI, "/var/backups")
	}

	var body snapshotsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Snapshots) != 2 {
		t.Fatalf("snapshots = %+v, want 2", body.Snapshots)
	}
	newest := body.Snapshots[0]
	if newest.Key != "20260601T120000Z.age" {
		t.Errorf("newest key = %q, want the first entry core returned", newest.Key)
	}
	if newest.SizeBytes != 4096 {
		t.Errorf("newest sizeBytes = %d, want 4096", newest.SizeBytes)
	}
	if !newest.Modified.Equal(modified) {
		t.Errorf("newest modified = %v, want %v", newest.Modified, modified)
	}
	if newest.Age != "1h30m0s" {
		t.Errorf("newest age = %q, want %q", newest.Age, "1h30m0s")
	}
}

// An empty destination is an empty list and a 200, not an error: an
// operator who has configured a destination but not yet run `backup` has
// nothing wrong to report.
func TestHandleSnapshotsEmptyHistoryIsNotAnError(t *testing.T) {
	s := newTestServer()
	s.openDestination = func(uri string) (blob.Adapter, error) { return nil, nil }
	s.snapshotHistory = func(context.Context, blob.Adapter, time.Time) ([]backup.Snapshot, error) {
		return nil, nil
	}

	rec := doSnapshots(t, s, "to=/var/backups")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body snapshotsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Snapshots == nil {
		t.Fatal("snapshots is null, want an empty array so the dashboard can render it directly")
	}
	if len(body.Snapshots) != 0 {
		t.Fatalf("snapshots = %+v, want empty", body.Snapshots)
	}
}

// New wires the real core read, so the endpoint the dashboard calls is
// backed by core's backup.History rather than anything assembled here.
func TestNewWiresRealSnapshotHistory(t *testing.T) {
	s := New()
	if s.openDestination == nil || s.snapshotHistory == nil {
		t.Fatal("New: snapshot history is not wired")
	}

	dir := t.TempDir()
	dest, err := s.openDestination(dir)
	if err != nil {
		t.Fatalf("openDestination: %v", err)
	}
	snapshots, err := s.snapshotHistory(context.Background(), dest, time.Now())
	if err != nil {
		t.Fatalf("snapshotHistory: %v", err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("snapshots = %+v, want none for an empty directory", snapshots)
	}
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return body["error"]
}
