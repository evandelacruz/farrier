package main

import (
	"fmt"
	"io"
	"os"

	"github.com/evandelacruz/farrier/internal/core/events"
)

// runJob renders job's CORE-002 event stream to stdout while operation
// runs (XCUT-002) — the same event stream the dashboard renders over SSE
// (spec.md "Long-running operations"). Every CLI command that wraps a job
// around a long-running operation calls this instead of threading its own
// subscribe/print/wait boilerplate, so the rendering path is shared and
// tested once.
//
// operation must drive job to its terminal event (directly, or by calling
// into a core package that does) before it returns; runJob waits for the
// printer to drain the stream before returning operation's error, so a
// command's own error message on failure never races the last rendered
// event.
func runJob(job *events.Job, operation func() error) error {
	return runJobTo(os.Stdout, job, operation)
}

// runJobTo is runJob with the render target as a parameter, so tests can
// assert on rendered output without touching os.Stdout.
func runJobTo(w io.Writer, job *events.Job, operation func() error) error {
	sub, cancel := job.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		printEvents(w, sub)
		close(done)
	}()

	err := operation()
	<-done
	return err
}

// printEvents renders each event in ch to w as it arrives. A step event
// prints as "[step] state: detail". A whole-job event (empty step) prints
// as just its detail — the state is implied by which of Succeeded/Failed
// ended the job, and the command's own exit code already reflects that.
func printEvents(w io.Writer, ch <-chan events.Event) {
	for ev := range ch {
		if ev.Step == "" {
			fmt.Fprintf(w, "%s\n", ev.Detail)
			continue
		}
		fmt.Fprintf(w, "[%s] %s: %s\n", ev.Step, ev.State, ev.Detail)
	}
}
