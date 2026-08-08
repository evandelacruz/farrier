package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/events"
)

func TestPrintEventsRendersStepEvents(t *testing.T) {
	ch := make(chan events.Event, 1)
	ch <- events.Event{Step: "validate", State: events.StateStarted, Detail: "checking inputs"}
	close(ch)

	var buf bytes.Buffer
	printEvents(&buf, ch)

	want := "[validate] started: checking inputs\n"
	if got := buf.String(); got != want {
		t.Errorf("printEvents() = %q, want %q", got, want)
	}
}

func TestPrintEventsRendersTerminalEventAsDetailOnly(t *testing.T) {
	ch := make(chan events.Event, 1)
	ch <- events.Event{Step: "", State: events.StateSucceeded, Detail: "bundle created"}
	close(ch)

	var buf bytes.Buffer
	printEvents(&buf, ch)

	want := "bundle created\n"
	if got := buf.String(); got != want {
		t.Errorf("printEvents() = %q, want %q", got, want)
	}
}

func TestPrintEventsPreservesOrder(t *testing.T) {
	ch := make(chan events.Event, 3)
	ch <- events.Event{Step: "a", State: events.StateStarted, Detail: "first"}
	ch <- events.Event{Step: "a", State: events.StateSucceeded, Detail: "second"}
	ch <- events.Event{Step: "", State: events.StateSucceeded, Detail: "third"}
	close(ch)

	var buf bytes.Buffer
	printEvents(&buf, ch)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	want := []string{
		"[a] started: first",
		"[a] succeeded: second",
		"third",
	}
	if len(lines) != len(want) {
		t.Fatalf("printEvents() produced %d lines, want %d: %q", len(lines), len(want), buf.String())
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestRunJobToRendersEventsAndReturnsOperationError(t *testing.T) {
	job := events.NewJob()
	wantErr := errors.New("boom")

	var buf bytes.Buffer
	err := runJobTo(&buf, job, func() error {
		job.Started("step-one", "doing the thing")
		job.Emit("step-one", events.StateFailed, "it broke")
		job.Failed("boom")
		return wantErr
	})

	if !errors.Is(err, wantErr) {
		t.Errorf("runJobTo() error = %v, want %v", err, wantErr)
	}

	want := "[step-one] started: doing the thing\n[step-one] failed: it broke\nboom\n"
	if got := buf.String(); got != want {
		t.Errorf("runJobTo() rendered %q, want %q", got, want)
	}
}

func TestRunJobToWaitsForStreamToDrainBeforeReturning(t *testing.T) {
	job := events.NewJob()

	var buf bytes.Buffer
	err := runJobTo(&buf, job, func() error {
		for i := 0; i < 50; i++ {
			job.Started("step", "in progress")
			job.Emit("step", events.StateSucceeded, "done")
		}
		job.Succeeded("all done")
		return nil
	})

	if err != nil {
		t.Fatalf("runJobTo() error = %v, want nil", err)
	}

	// Every emitted event must have been rendered by the time runJobTo
	// returns — a command printing its own success/failure line right
	// after runJobTo must never race the last rendered job event.
	wantLines := 50*2 + 1
	gotLines := strings.Count(buf.String(), "\n")
	if gotLines != wantLines {
		t.Errorf("runJobTo() rendered %d lines, want %d", gotLines, wantLines)
	}
	if !strings.HasSuffix(buf.String(), "all done\n") {
		t.Errorf("runJobTo() output does not end with the terminal event: %q", buf.String())
	}
}
