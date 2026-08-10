package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

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

// ANSI escapes, empty when the target is not an interactive terminal so
// piped and redirected output stays plain text.
const (
	ansiReset = "\x1b[0m"
	ansiBold  = "\x1b[1m"
	ansiDim   = "\x1b[2m"
	ansiGreen = "\x1b[32m"
	ansiRed   = "\x1b[31m"
)

// renderer holds what the shape of the output depends on: whether escapes
// are wanted, how wide to wrap, and which step's group is currently open.
type renderer struct {
	w     io.Writer
	color bool
	width int
	// step is the step whose group is open. A new step closes the previous
	// group with a blank line and prints its own heading, so a step that
	// emits ten events — `init` reporting where every key went — names
	// itself once rather than ten times.
	step string
	// opened is false until the first group prints, so output does not
	// begin with a blank line.
	opened bool
}

// printEvents renders each event in ch to w as it arrives.
//
// Events are grouped by step. A step heading prints once, followed by that
// step's detail lines indented beneath it, with a blank line between
// groups — the previous format repeated "[step] state:" on every line,
// which turned a command like `init` into an undifferentiated wall.
//
// A whole-job event (empty step) is the terminal one, and prints as a
// banner: it is the line the operator is looking for, and the exit code
// alone does not say it on screen.
func printEvents(w io.Writer, ch <-chan events.Event) {
	r := &renderer{w: w, color: wantsColor(w), width: terminalWidth()}
	for ev := range ch {
		r.render(ev)
	}
}

func (r *renderer) render(ev events.Event) {
	if ev.Step == "" {
		r.terminal(ev)
		return
	}

	if ev.Step != r.step {
		if r.opened {
			fmt.Fprintln(r.w)
		}
		fmt.Fprintf(r.w, "%s\n", r.paint(ev.Step, ansiBold))
		r.step = ev.Step
		r.opened = true
	}

	switch ev.State {
	case events.StateStarted:
		// The started detail says what is about to happen; it is context
		// for the lines under it rather than a result, so it is dimmed and
		// carries no mark.
		r.detail("  ", r.paint(ev.Detail, ansiDim))
	case events.StateFailed:
		r.detail("  ", r.paint("✗ ", ansiRed+ansiBold)+r.paint(ev.Detail, ansiRed))
	default:
		r.detail("  ", r.paint("✓ ", ansiGreen)+ev.Detail)
	}
}

// terminal renders the whole-job event as a banner, separated from the
// steps above it. Failure is worth shouting about; success is worth being
// unmistakable, since several commands end with something the operator
// must act on.
func (r *renderer) terminal(ev events.Event) {
	if r.opened {
		fmt.Fprintln(r.w)
	}
	r.opened = true
	r.step = ""

	mark, color := "SUCCESS", ansiGreen
	if ev.State == events.StateFailed {
		mark, color = "FAILED", ansiRed
	}
	fmt.Fprintf(r.w, "%s\n", r.paint(mark, color+ansiBold))
	r.detail("", ev.Detail)
}

// detail writes text under indent, wrapping to the render width so a long
// message reads as a paragraph rather than one line that scrolls sideways.
// Continuation lines align under the first, so wrapped text stays visually
// inside its group.
func (r *renderer) detail(indent, text string) {
	for i, line := range wrap(text, r.width-len(indent)) {
		pad := indent
		if i > 0 {
			pad = indent + "  "
		}
		fmt.Fprintf(r.w, "%s%s\n", pad, line)
	}
}

// paint wraps s in an ANSI escape when the target is a terminal, and
// returns it untouched otherwise.
func (r *renderer) paint(s, escape string) string {
	if !r.color {
		return s
	}
	return escape + s + ansiReset
}

// wrap breaks text at width, on spaces, without splitting a word. Escape
// sequences are not counted, so it is only ever called on text that has
// not been painted yet. A width of zero or less disables wrapping.
func wrap(text string, width int) []string {
	fields := strings.Fields(text)
	if width <= 0 || len(fields) == 0 {
		return []string{text}
	}

	lines := []string{}
	line := fields[0]
	for _, word := range fields[1:] {
		if len(line)+1+len(word) > width {
			lines = append(lines, line)
			line = word
			continue
		}
		line += " " + word
	}
	return append(lines, line)
}

// wantsColor reports whether escapes should be emitted. Three things turn
// them off, and each matters: NO_COLOR is the cross-tool convention
// (no-color.org), a writer that is not a character device is a pipe, a
// file, or a test buffer, and TERM=dumb is a terminal saying so itself.
func wantsColor(w io.Writer) bool {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// terminalWidth is the wrap width. COLUMNS is honored when the shell
// exports it; otherwise 80, which is narrow enough to be safe anywhere and
// is what every terminal defaults to.
func terminalWidth() int {
	if cols, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && cols > 20 {
		return cols
	}
	return 80
}
