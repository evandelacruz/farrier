package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCloudflare is a minimal stand-in for the Cloudflare DNS records API:
// enough of /zones/{zoneID}/dns_records (list by name, create, delete) to
// exercise CloudflareDriver's request shapes and response handling.
type fakeCloudflare struct {
	mu          sync.Mutex
	zoneID      string
	wantToken   string
	records     map[string]cfRecord
	nextID      int
	failMessage string // when set, every call returns success:false
}

func newFakeCloudflare(zoneID, token string) *fakeCloudflare {
	return &fakeCloudflare{zoneID: zoneID, wantToken: token, records: map[string]cfRecord{}}
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+f.wantToken {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(cfResponse{Success: false, Errors: []cfError{{Code: 9109, Message: "invalid token"}}})
		return
	}

	prefix := "/zones/" + f.zoneID + "/dns_records"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failMessage != "" {
		json.NewEncoder(w).Encode(cfResponse{Success: false, Errors: []cfError{{Code: 1000, Message: f.failMessage}}})
		return
	}

	switch {
	case rest == "" && r.Method == http.MethodGet:
		name := r.URL.Query().Get("name")
		var matched []cfRecord
		for _, rec := range f.records {
			if rec.Name == name {
				matched = append(matched, rec)
			}
		}
		f.reply(w, matched)
	case rest == "" && r.Method == http.MethodPost:
		var rec cfRecord
		json.NewDecoder(r.Body).Decode(&rec)
		f.nextID++
		rec.ID = strconv.Itoa(f.nextID)
		f.records[rec.ID] = rec
		f.reply(w, rec)
	case strings.HasPrefix(rest, "/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(rest, "/")
		delete(f.records, id)
		f.reply(w, map[string]string{"id": id})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeCloudflare) reply(w http.ResponseWriter, result any) {
	body, _ := json.Marshal(result)
	json.NewEncoder(w).Encode(cfResponse{Success: true, Result: body})
}

func newTestCloudflareDriver(t *testing.T, fake *fakeCloudflare) *CloudflareDriver {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	d, err := NewCloudflare(CloudflareConfig{
		ZoneID:   fake.zoneID,
		APIToken: fake.wantToken,
		BaseURL:  server.URL,
	})
	if err != nil {
		t.Fatalf("NewCloudflare: %v", err)
	}
	return d
}

func TestCloudflareSetCreatesRecord(t *testing.T) {
	fake := newFakeCloudflare("zone1", "token1")
	d := newTestCloudflareDriver(t, fake)

	if err := d.Set(context.Background(), "app.example.com", "203.0.113.10", 60*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.records) != 1 {
		t.Fatalf("records = %d, want 1", len(fake.records))
	}
	for _, rec := range fake.records {
		if rec.Type != "A" || rec.Content != "203.0.113.10" || rec.TTL != 60 {
			t.Fatalf("record = %+v, want type A, content 203.0.113.10, ttl 60", rec)
		}
	}
}

func TestCloudflareSetReplacesExistingRecordOfDifferentType(t *testing.T) {
	fake := newFakeCloudflare("zone1", "token1")
	fake.records["1"] = cfRecord{ID: "1", Type: "CNAME", Name: "app.example.com", Content: "standby.example.com", TTL: 60}
	fake.nextID = 1
	d := newTestCloudflareDriver(t, fake)

	if err := d.Set(context.Background(), "app.example.com", "203.0.113.10", 60*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.records) != 1 {
		t.Fatalf("records = %d, want 1", len(fake.records))
	}
	for _, rec := range fake.records {
		if rec.ID == "1" {
			t.Fatalf("stale CNAME record %+v was not deleted", rec)
		}
		if rec.Type != "A" || rec.Content != "203.0.113.10" {
			t.Fatalf("record = %+v, want fresh A record for 203.0.113.10", rec)
		}
	}
}

func TestCloudflareSetHostnameValueCreatesCNAME(t *testing.T) {
	fake := newFakeCloudflare("zone1", "token1")
	d := newTestCloudflareDriver(t, fake)

	if err := d.Set(context.Background(), "app.example.com", "standby.example.com", 60*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, rec := range fake.records {
		if rec.Type != "CNAME" {
			t.Fatalf("record type = %q, want CNAME", rec.Type)
		}
	}
}

func TestCloudflareDeleteRemovesRecord(t *testing.T) {
	fake := newFakeCloudflare("zone1", "token1")
	fake.records["1"] = cfRecord{ID: "1", Type: "A", Name: "app.example.com", Content: "203.0.113.10", TTL: 60}
	fake.nextID = 1
	d := newTestCloudflareDriver(t, fake)

	if err := d.Delete(context.Background(), "app.example.com"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.records) != 0 {
		t.Fatalf("records = %d, want 0", len(fake.records))
	}
}

func TestCloudflareDeleteAbsentRecordIsNotAnError(t *testing.T) {
	fake := newFakeCloudflare("zone1", "token1")
	d := newTestCloudflareDriver(t, fake)

	if err := d.Delete(context.Background(), "app.example.com"); err != nil {
		t.Fatalf("Delete on absent record: %v", err)
	}
}

func TestCloudflareAPIErrorPropagates(t *testing.T) {
	fake := newFakeCloudflare("zone1", "token1")
	fake.failMessage = "zone is paused"
	d := newTestCloudflareDriver(t, fake)

	err := d.Set(context.Background(), "app.example.com", "203.0.113.10", 60*time.Second)
	if err == nil || !strings.Contains(err.Error(), "zone is paused") {
		t.Fatalf("Set error = %v, want it to mention %q", err, "zone is paused")
	}
}

func TestCloudflareInvalidTokenErrors(t *testing.T) {
	fake := newFakeCloudflare("zone1", "token1")
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	d, err := NewCloudflare(CloudflareConfig{ZoneID: "zone1", APIToken: "wrong-token", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewCloudflare: %v", err)
	}
	if err := d.Set(context.Background(), "app.example.com", "203.0.113.10", 60*time.Second); err == nil {
		t.Fatal("Set with wrong token: want error, got nil")
	}
}

func TestNewCloudflareRequiresZoneIDAndToken(t *testing.T) {
	if _, err := NewCloudflare(CloudflareConfig{APIToken: "t"}); err == nil {
		t.Fatal("NewCloudflare with no zone id: want error, got nil")
	}
	if _, err := NewCloudflare(CloudflareConfig{ZoneID: "z"}); err == nil {
		t.Fatal("NewCloudflare with no token: want error, got nil")
	}
}

func TestCloudflareSetValidatesArgs(t *testing.T) {
	fake := newFakeCloudflare("zone1", "token1")
	d := newTestCloudflareDriver(t, fake)

	if err := d.Set(context.Background(), "", "203.0.113.10", 60*time.Second); err == nil {
		t.Fatal("Set with empty record: want error, got nil")
	}
	if err := d.Set(context.Background(), "app.example.com", "", 60*time.Second); err == nil {
		t.Fatal("Set with empty value: want error, got nil")
	}
	if err := d.Set(context.Background(), "app.example.com", "203.0.113.10", 0); err == nil {
		t.Fatal("Set with zero ttl: want error, got nil")
	}
}
