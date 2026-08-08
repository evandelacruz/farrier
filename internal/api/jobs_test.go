package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/events"
)

func TestHandleJobEventsNotFound(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/jobs/nope/events", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestHandleJobEventsStreams drives the SSE endpoint through a real HTTP
// server — httptest.ResponseRecorder does not support the streaming
// Flush/read-while-write behavior the handler relies on.
func TestHandleJobEventsStreams(t *testing.T) {
	s := newTestServer()
	job := s.jobs.New()

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/jobs/" + job.ID() + "/events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	job.Started("clone", "cloning")
	job.Succeeded("done")

	reader := bufio.NewReader(resp.Body)
	var got []events.Event
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE frame: %v", err)
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev events.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		got = append(got, ev)
	}

	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Step != "clone" || got[0].State != events.StateStarted {
		t.Errorf("event 0 = %+v, want step=clone state=started", got[0])
	}
	if got[1].Step != "" || got[1].State != events.StateSucceeded {
		t.Errorf("event 1 = %+v, want step=\"\" state=succeeded", got[1])
	}
	for _, ev := range got {
		if ev.JobID != job.ID() {
			t.Errorf("event JobID = %q, want %q", ev.JobID, job.ID())
		}
	}
}

// TestHandleJobEventsReplaysHistory covers a late subscriber attaching
// after the job already finished: it must still see the full event
// history, then the stream closes.
func TestHandleJobEventsReplaysHistory(t *testing.T) {
	s := newTestServer()
	job := s.jobs.New()
	job.Started("clone", "cloning")
	job.Succeeded("done")

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/jobs/" + job.ID() + "/events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	var got []events.Event
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev events.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		got = append(got, ev)
	}

	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
}
