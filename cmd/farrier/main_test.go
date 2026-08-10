package main

import (
	"os"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout replaced by a pipe and returns what it
// wrote. printUsage takes an *os.File precisely so help can go to stdout while
// usage errors go to stderr, which is the distinction these tests check.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	fn()
	w.Close()
	os.Stdout = saved
	return <-done
}

// Asking for help is not a usage error. All three spellings must exit 0 and
// write to stdout: `farrier --help | less` shows nothing if help goes to
// stderr, and a wrapper script reads a nonzero exit as a failed command.
func TestHelpFlagsExitZeroAndWriteToStdout(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var code int
			out := captureStdout(t, func() { code = run([]string{arg}) })
			if code != 0 {
				t.Errorf("run(%q) = %d, want 0", arg, code)
			}
			if !strings.Contains(out, "usage: farrier") {
				t.Errorf("run(%q) wrote no usage to stdout: %q", arg, out)
			}
			for name := range commands {
				if !strings.Contains(out, name) {
					t.Errorf("run(%q) omitted command %q", arg, name)
				}
			}
		})
	}
}

// No arguments is a usage error, unlike an explicit help request.
func TestNoArgumentsIsAUsageError(t *testing.T) {
	if code := run(nil); code != 2 {
		t.Errorf("run(nil) = %d, want 2", code)
	}
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	if code := run([]string{"nope"}); code != 2 {
		t.Errorf("run([nope]) = %d, want 2", code)
	}
	if code := run([]string{"help", "nope"}); code != 2 {
		t.Errorf("run([help nope]) = %d, want 2", code)
	}
}

// `farrier help <command>` hands the runner -h, which it answers by printing
// its flags and returning 2 — it cannot tell a help request from a parse
// error. This call site can, so the exit code is 0.
//
// The exit code is the cheap half. The runner writes its usage to os.Stderr,
// so asserting only the code passed while `farrier help up | less` printed
// nothing — the exact failure this command was added to fix. Assert the
// output lands on stdout, and that it is the command's own flags rather than
// the top-level usage.
func TestHelpForACommandWritesItsFlagsToStdout(t *testing.T) {
	for name := range commands {
		t.Run(name, func(t *testing.T) {
			var code int
			out := captureStdout(t, func() { code = run([]string{"help", name}) })
			if code != 0 {
				t.Errorf("run([help %s]) = %d, want 0", name, code)
			}
			if !strings.Contains(out, "Usage of "+name) {
				t.Errorf("run([help %s]) wrote no flag usage to stdout: %q", name, out)
			}
		})
	}
}

// The swap helpFor makes must not outlive the call: everything after it —
// every error path in every command — writes diagnostics to os.Stderr and
// would silently land on stdout instead.
func TestHelpForRestoresStderr(t *testing.T) {
	before := os.Stderr
	captureStdout(t, func() { run([]string{"help", "up"}) })
	if os.Stderr != before {
		t.Error("helpFor left os.Stderr pointed somewhere else")
	}
	// The unknown-command path returns before the swap; check it too, since a
	// future refactor could easily move the swap above that branch.
	captureStdout(t, func() { run([]string{"help", "nope"}) })
	if os.Stderr != before {
		t.Error("helpFor left os.Stderr pointed somewhere else after an unknown command")
	}
}

// The list used to range the commands map, so it printed in a different order
// every run. Order comes from commandOrder now, and the two must not drift:
// a command added to one and not the other either never prints or panics on a
// missing map entry.
func TestCommandOrderCoversEveryCommandExactlyOnce(t *testing.T) {
	if len(commandOrder) != len(commands) {
		t.Fatalf("commandOrder has %d entries, commands has %d", len(commandOrder), len(commands))
	}
	seen := make(map[string]bool, len(commandOrder))
	for _, name := range commandOrder {
		if _, ok := commands[name]; !ok {
			t.Errorf("commandOrder lists %q, which is not a command", name)
		}
		if seen[name] {
			t.Errorf("commandOrder lists %q twice", name)
		}
		seen[name] = true
	}
}

// Every command needs a summary; the list is the only place most operators
// will ever see what a command does.
func TestEveryCommandHasASummary(t *testing.T) {
	for name, cmd := range commands {
		if strings.TrimSpace(cmd.summary) == "" {
			t.Errorf("command %q has no summary", name)
		}
		if cmd.run == nil {
			t.Errorf("command %q has no runner", name)
		}
	}
}
