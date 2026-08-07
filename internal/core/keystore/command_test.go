package keystore

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain lets this test binary also play the part of an operator-specified
// command: when FARRIER_KEYSTORE_HELPER is set, it runs the requested helper
// behavior and exits instead of running the test suite. This exercises
// CommandResolver against a real subprocess without shipping a separate
// script — the same pattern internal/core/driver's exec_test.go uses.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FARRIER_KEYSTORE_HELPER"); mode != "" {
		os.Exit(runHelper(mode))
	}
	os.Exit(m.Run())
}

func runHelper(mode string) int {
	switch mode {
	case "echo":
		fmt.Fprint(os.Stdout, "resolved-secret\n")
		return 0
	case "echo-arg":
		fmt.Fprint(os.Stdout, os.Args[len(os.Args)-1])
		return 0
	case "echo-env":
		fmt.Fprint(os.Stdout, os.Getenv(keyNameEnvVar))
		return 0
	case "fail-with-partial-stdout":
		fmt.Fprint(os.Stdout, "partial-secret-fragment")
		fmt.Fprint(os.Stderr, "boom")
		return 1
	case "sleep":
		time.Sleep(5 * time.Second)
		return 0
	default:
		fmt.Fprintln(os.Stderr, "runHelper: unknown mode", mode)
		return 2
	}
}

// helperCommand returns a CommandResolver that re-invokes this test binary
// with FARRIER_KEYSTORE_HELPER set to mode, so Resolve runs runHelper above.
func helperCommand(t *testing.T, mode string, extraArgs ...string) *CommandResolver {
	t.Helper()
	t.Setenv("FARRIER_KEYSTORE_HELPER", mode)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	args := append([]string{"-test.run=^TestMain$"}, extraArgs...)
	return &CommandResolver{Command: exe, Args: args}
}

func TestCommandResolveCapturesStdout(t *testing.T) {
	r := helperCommand(t, "echo")
	got, err := r.Resolve(context.Background(), "any-key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := "resolved-secret"; got.Reveal() != want {
		t.Fatalf("Reveal() = %q, want %q", got.Reveal(), want)
	}
}

func TestCommandResolveSubstitutesKeyPlaceholder(t *testing.T) {
	r := helperCommand(t, "echo-arg", keyNamePlaceholder)
	got, err := r.Resolve(context.Background(), "lfs-jwt-secret")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := "lfs-jwt-secret"; got.Reveal() != want {
		t.Fatalf("Reveal() = %q, want %q", got.Reveal(), want)
	}
}

func TestCommandResolveExposesKeyNameEnvVar(t *testing.T) {
	r := helperCommand(t, "echo-env")
	got, err := r.Resolve(context.Background(), "internal-token")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := "internal-token"; got.Reveal() != want {
		t.Fatalf("Reveal() = %q, want %q", got.Reveal(), want)
	}
}

func TestCommandResolveFailureDoesNotLeakStdout(t *testing.T) {
	r := helperCommand(t, "fail-with-partial-stdout")
	_, err := r.Resolve(context.Background(), "any-key")
	if err == nil {
		t.Fatal("Resolve: want error, got nil")
	}
	if strings.Contains(err.Error(), "partial-secret-fragment") {
		t.Fatalf("error %q leaks partially captured stdout", err.Error())
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error %q should surface stderr diagnostic", err.Error())
	}
}

func TestCommandResolveEmptyKeyNameErrors(t *testing.T) {
	r := helperCommand(t, "echo")
	if _, err := r.Resolve(context.Background(), ""); err == nil {
		t.Fatal("Resolve: want error for empty key name, got nil")
	}
}

func TestNewCommandRejectsEmptyCommand(t *testing.T) {
	if _, err := NewCommand(""); err == nil {
		t.Fatal("NewCommand: want error for empty command, got nil")
	}
}

func TestCommandResolveContextCanceled(t *testing.T) {
	r := helperCommand(t, "sleep")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := r.Resolve(ctx, "any-key")
	if err == nil {
		t.Fatal("Resolve: want error, got nil")
	}
}
