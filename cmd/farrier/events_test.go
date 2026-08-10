package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/events"
)

// render drains a fixed set of events through printEvents into a buffer.
// A bytes.Buffer is not a character device, so every assertion below sees
// plain text — which is also what CI logs and `farrier init > log` see.
func render(evs ...events.Event) string {
	ch := make(chan events.Event, len(evs))
	for _, ev := range evs {
		ch <- ev
	}
	close(ch)

	var buf bytes.Buffer
	printEvents(&buf, ch)
	return buf.String()
}

func TestPrintEventsNamesAStepOnce(t *testing.T) {
	got := render(
		events.Event{Step: "report-key-material", State: events.StateStarted, Detail: "where each key went"},
		events.Event{Step: "report-key-material", State: events.StateSucceeded, Detail: "one"},
		events.Event{Step: "report-key-material", State: events.StateSucceeded, Detail: "two"},
	)
	if n := strings.Count(got, "report-key-material"); n != 1 {
		t.Errorf("step name appears %d times, want 1:\n%s", n, got)
	}
	for _, want := range []string{"where each key went", "✓ one", "✓ two"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestPrintEventsSeparatesStepGroups(t *testing.T) {
	got := render(
		events.Event{Step: "a", State: events.StateSucceeded, Detail: "first"},
		events.Event{Step: "b", State: events.StateSucceeded, Detail: "second"},
	)
	if !strings.Contains(got, "\n\nb\n") {
		t.Errorf("no blank line between groups:\n%q", got)
	}
	// ...but never one before the first group, which would open every
	// command with a stray empty line.
	if strings.HasPrefix(got, "\n") {
		t.Errorf("output starts with a blank line:\n%q", got)
	}
}

func TestPrintEventsMarksSuccessAndFailure(t *testing.T) {
	got := render(
		events.Event{Step: "a", State: events.StateSucceeded, Detail: "worked"},
		events.Event{Step: "a", State: events.StateFailed, Detail: "broke"},
	)
	if !strings.Contains(got, "✓ worked") || !strings.Contains(got, "✗ broke") {
		t.Errorf("missing success/failure marks:\n%s", got)
	}
}

// The terminal event is the line the operator is looking for, and the exit
// code does not say it on screen.
func TestPrintEventsBannersTheTerminalEvent(t *testing.T) {
	ok := render(events.Event{State: events.StateSucceeded, Detail: "bundle created"})
	if !strings.Contains(ok, "SUCCESS") || !strings.Contains(ok, "bundle created") {
		t.Errorf("success banner missing:\n%s", ok)
	}

	bad := render(events.Event{State: events.StateFailed, Detail: "could not resolve"})
	if !strings.Contains(bad, "FAILED") || !strings.Contains(bad, "could not resolve") {
		t.Errorf("failure banner missing:\n%s", bad)
	}
}

func TestPrintEventsPreservesOrder(t *testing.T) {
	got := render(
		events.Event{Step: "a", State: events.StateStarted, Detail: "first"},
		events.Event{Step: "a", State: events.StateSucceeded, Detail: "second"},
		events.Event{State: events.StateSucceeded, Detail: "third"},
	)
	first, second, third := strings.Index(got, "first"), strings.Index(got, "second"), strings.Index(got, "third")
	if first < 0 || second < first || third < second {
		t.Errorf("events out of order (%d, %d, %d):\n%s", first, second, third, got)
	}
}

// Escapes must never reach a pipe, a file, or a CI log. This is the whole
// reason wantsColor exists, and a regression would be invisible in a
// terminal — where it looks correct — while corrupting every log.
func TestPrintEventsWritesNoEscapesToANonTerminal(t *testing.T) {
	got := render(
		events.Event{Step: "a", State: events.StateStarted, Detail: "one"},
		events.Event{Step: "a", State: events.StateFailed, Detail: "two"},
		events.Event{State: events.StateSucceeded, Detail: "three"},
	)
	if strings.Contains(got, "\x1b[") {
		t.Errorf("ANSI escape written to a non-terminal:\n%q", got)
	}
}

func TestWantsColorRespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if wantsColor(os.Stdout) {
		t.Error("wantsColor() = true with NO_COLOR set")
	}
}

func TestWantsColorRespectsDumbTerminals(t *testing.T) {
	t.Setenv("TERM", "dumb")
	if wantsColor(os.Stdout) {
		t.Error("wantsColor() = true with TERM=dumb")
	}
}

func TestWrapBreaksOnSpacesWithoutSplittingWords(t *testing.T) {
	lines := wrap("the age backup key is the one unrecoverable loss", 20)
	if len(lines) < 2 {
		t.Fatalf("wrap() did not wrap: %q", lines)
	}
	for _, line := range lines {
		if len(line) > 20 {
			t.Errorf("line exceeds width: %q", line)
		}
	}
	if strings.Join(lines, " ") != "the age backup key is the one unrecoverable loss" {
		t.Errorf("wrap() lost or reordered words: %q", lines)
	}
}

// A word longer than the width still gets its own line rather than being
// cut — a filesystem path is the realistic case, and truncating one would
// be worse than letting it run long.
func TestWrapKeepsAnOverlongWordIntact(t *testing.T) {
	long := "/Users/evan/.farrier/keys/forgejo_internal_token"
	lines := wrap("stored at "+long, 20)
	if !strings.Contains(strings.Join(lines, "\n"), long) {
		t.Errorf("wrap() split an overlong word: %q", lines)
	}
}

func TestRunJobToRendersAndReturnsTheOperationError(t *testing.T) {
	job := events.NewJob()
	want := errors.New("boom")

	var buf bytes.Buffer
	err := runJobTo(&buf, job, func() error {
		job.Started("step", "starting")
		job.Emit("step", events.StateFailed, want.Error())
		job.Failed(want.Error())
		return want
	})

	if !errors.Is(err, want) {
		t.Errorf("runJobTo() error = %v, want %v", err, want)
	}
	if !strings.Contains(buf.String(), "✗ boom") {
		t.Errorf("output missing the failure:\n%s", buf.String())
	}
}
