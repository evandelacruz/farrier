package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain lets this test binary also play the part of a third-party driver
// executable: when FARRIER_DRIVER_HELPER is set, it runs the requested
// helper behavior and exits instead of running the test suite. This
// exercises Exec against a real subprocess without shipping a separate
// script or binary — the same pattern os/exec's own tests use.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FARRIER_DRIVER_HELPER"); mode != "" {
		os.Exit(runHelper(mode))
	}
	os.Exit(m.Run())
}

func runHelper(mode string) int {
	switch mode {
	case "echo":
		var req Request
		if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return encodeAndExit(Response{OK: true, Result: req.Params})
	case "fail":
		return encodeAndExit(Response{OK: false, Error: "boom"})
	case "fail-no-message":
		return encodeAndExit(Response{OK: false})
	case "crash":
		fmt.Fprintln(os.Stderr, "helper: exploded")
		return 1
	case "badjson":
		fmt.Fprint(os.Stdout, "{not json")
		return 0
	case "sleep":
		time.Sleep(5 * time.Second)
		return 0
	default:
		fmt.Fprintln(os.Stderr, "runHelper: unknown mode", mode)
		return 2
	}
}

func encodeAndExit(resp Response) int {
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// helper returns an Exec that re-invokes this test binary with
// FARRIER_DRIVER_HELPER set to mode, so Invoke talks to runHelper above.
func helper(t *testing.T, mode string) Exec {
	t.Helper()
	t.Setenv("FARRIER_DRIVER_HELPER", mode)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return Exec{Path: exe, Args: []string{"-test.run=^TestMain$"}}
}

func TestExecInvokeRoundTripsParamsAsResult(t *testing.T) {
	e := helper(t, "echo")

	type payload struct {
		Name string `json:"name"`
	}
	var got payload
	if err := e.Invoke(context.Background(), "set", payload{Name: "example.com"}, &got); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got.Name != "example.com" {
		t.Fatalf("got %+v, want Name=example.com", got)
	}
}

func TestExecInvokeNoParamsNoResult(t *testing.T) {
	e := helper(t, "echo")

	if err := e.Invoke(context.Background(), "resolve", nil, nil); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
}

func TestExecInvokeDriverError(t *testing.T) {
	e := helper(t, "fail")

	err := e.Invoke(context.Background(), "set", nil, nil)
	if err == nil {
		t.Fatal("Invoke: want error, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Invoke error = %q, want it to contain %q", err.Error(), "boom")
	}
}

func TestExecInvokeDriverErrorWithNoMessage(t *testing.T) {
	e := helper(t, "fail-no-message")

	err := e.Invoke(context.Background(), "set", nil, nil)
	if err == nil {
		t.Fatal("Invoke: want error, got nil")
	}
}

func TestExecInvokeNonzeroExitSurfacesStderr(t *testing.T) {
	e := helper(t, "crash")

	err := e.Invoke(context.Background(), "set", nil, nil)
	if err == nil {
		t.Fatal("Invoke: want error, got nil")
	}
	if !strings.Contains(err.Error(), "exploded") {
		t.Fatalf("Invoke error = %q, want it to contain driver stderr %q", err.Error(), "exploded")
	}
}

func TestExecInvokeInvalidResponseJSON(t *testing.T) {
	e := helper(t, "badjson")

	err := e.Invoke(context.Background(), "set", nil, nil)
	if err == nil {
		t.Fatal("Invoke: want error, got nil")
	}
}

func TestExecInvokeTimeout(t *testing.T) {
	e := helper(t, "sleep")
	e.Timeout = 100 * time.Millisecond

	err := e.Invoke(context.Background(), "set", nil, nil)
	if err == nil {
		t.Fatal("Invoke: want error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Invoke error = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

func TestExecInvokeContextCanceled(t *testing.T) {
	e := helper(t, "sleep")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := e.Invoke(ctx, "set", nil, nil)
	if err == nil {
		t.Fatal("Invoke: want error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Invoke error = %v, want it to wrap context.Canceled", err)
	}
}

func TestExecInvokeMissingPath(t *testing.T) {
	e := Exec{}
	if err := e.Invoke(context.Background(), "set", nil, nil); err == nil {
		t.Fatal("Invoke: want error for empty Path, got nil")
	}
}

func TestExecInvokeParamsEncodeError(t *testing.T) {
	e := helper(t, "echo")

	// A channel value cannot be JSON-encoded, so this must fail before a
	// process is ever started.
	err := e.Invoke(context.Background(), "set", make(chan int), nil)
	if err == nil {
		t.Fatal("Invoke: want error, got nil")
	}
}

func TestExecInvokeResultDecodeError(t *testing.T) {
	e := helper(t, "echo")

	// echo returns whatever params it was given as the result; a string
	// param cannot decode into an int target.
	var into int
	err := e.Invoke(context.Background(), "set", "not-a-number", &into)
	if err == nil {
		t.Fatal("Invoke: want error, got nil")
	}
}
