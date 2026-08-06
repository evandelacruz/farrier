package events

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func drain(t *testing.T, ch <-chan Event, timeout time.Duration) []Event {
	t.Helper()
	var got []Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out waiting for channel; got %d events so far", len(got))
		}
	}
}

func TestEmitSchemaAndOrder(t *testing.T) {
	j := NewJob()
	j.Started("clone", "cloning repository")
	j.Emit("clone", StateSucceeded, "clone complete")
	j.Succeeded("job complete")

	got := j.Events()
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	for i, ev := range got {
		if ev.JobID != j.ID() {
			t.Errorf("event %d: JobID = %q, want %q", i, ev.JobID, j.ID())
		}
		if ev.Timestamp.IsZero() {
			t.Errorf("event %d: Timestamp is zero", i)
		}
	}
	if got[1].Timestamp.Before(got[0].Timestamp) {
		t.Errorf("events not in emission order: %v before %v", got[1].Timestamp, got[0].Timestamp)
	}
	if got[0].Step != "clone" || got[0].State != StateStarted {
		t.Errorf("event 0 = %+v, want step=clone state=started", got[0])
	}
	if got[2].Step != "" || got[2].State != StateSucceeded {
		t.Errorf("event 2 = %+v, want step=\"\" state=succeeded", got[2])
	}

	raw, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, field := range []string{"jobId", "step", "state", "detail", "timestamp"} {
		if _, ok := m[field]; !ok {
			t.Errorf("marshaled event missing field %q: %s", field, raw)
		}
	}
}

func TestJobDoneAfterTerminalEvent(t *testing.T) {
	j := NewJob()
	if j.Done() {
		t.Fatal("Done() = true before any event")
	}
	j.Started("up", "starting")
	if j.Done() {
		t.Fatal("Done() = true after a non-terminal event")
	}
	j.Succeeded("done")
	if !j.Done() {
		t.Fatal("Done() = false after a terminal event")
	}
}

func TestEmitAfterTerminalPanics(t *testing.T) {
	j := NewJob()
	j.Failed("boom")

	defer func() {
		if recover() == nil {
			t.Fatal("Emit after terminal event did not panic")
		}
	}()
	j.Started("retry", "should not happen")
}

func TestSubscribeBeforeEmitReceivesLiveEvents(t *testing.T) {
	j := NewJob()
	ch, cancel := j.Subscribe()
	defer cancel()

	j.Started("up", "starting")
	j.Succeeded("done")

	got := drain(t, ch, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	if got[0].State != StateStarted || got[1].State != StateSucceeded {
		t.Errorf("got states %v, %v; want started, succeeded", got[0].State, got[1].State)
	}
}

func TestSubscribeMidStreamGetsBacklogThenLive(t *testing.T) {
	j := NewJob()
	j.Started("up", "starting")

	ch, cancel := j.Subscribe()
	defer cancel()

	j.Succeeded("done")

	got := drain(t, ch, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (backlog + live): %+v", len(got), got)
	}
	if got[0].State != StateStarted {
		t.Errorf("first event = %v, want the replayed started event", got[0])
	}
	if got[1].State != StateSucceeded {
		t.Errorf("second event = %v, want the live succeeded event", got[1])
	}
}

func TestSubscribeAfterDoneReplaysAndCloses(t *testing.T) {
	j := NewJob()
	j.Started("up", "starting")
	j.Succeeded("done")

	ch, cancel := j.Subscribe()
	defer cancel()

	got := drain(t, ch, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
}

func TestMultipleSubscribersEachGetFullStream(t *testing.T) {
	j := NewJob()

	const n = 5
	var chans []<-chan Event
	var cancels []func()
	for i := 0; i < n; i++ {
		ch, cancel := j.Subscribe()
		chans = append(chans, ch)
		cancels = append(cancels, cancel)
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()

	j.Started("up", "starting")
	j.Succeeded("done")

	var wg sync.WaitGroup
	for _, ch := range chans {
		wg.Add(1)
		go func(ch <-chan Event) {
			defer wg.Done()
			got := drain(t, ch, 2*time.Second)
			if len(got) != 2 {
				t.Errorf("subscriber got %d events, want 2", len(got))
			}
		}(ch)
	}
	wg.Wait()
}

func TestCancelStopsDeliveryWithoutBlockingEmit(t *testing.T) {
	j := NewJob()
	ch, cancel := j.Subscribe()
	cancel()

	// A canceled subscriber must not block Emit, and the job must still be
	// able to reach a terminal state normally.
	done := make(chan struct{})
	go func() {
		j.Started("up", "starting")
		j.Succeeded("done")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked after subscriber was canceled")
	}

	select {
	case _, ok := <-ch:
		if ok {
			// A pending backlog delivery before cancel took effect is
			// acceptable; draining to closed is what matters.
			drain(t, ch, 2*time.Second)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled subscriber channel never closed")
	}
}

func TestIDsAreUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := NewJob().ID()
		if seen[id] {
			t.Fatalf("duplicate job id %q", id)
		}
		seen[id] = true
	}
}
