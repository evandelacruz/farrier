package keystore

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCommandDriverResolveUsesKeyNameEnvVar(t *testing.T) {
	d := CommandDriver{Command: `printf '%s-secret' "$FARRIER_KEY_NAME"`}
	got, err := d.Resolve(context.Background(), "forgejo_secret_key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Reveal() != "forgejo_secret_key-secret" {
		t.Fatalf("Reveal() = %q, want %q", got.Reveal(), "forgejo_secret_key-secret")
	}
}

func TestCommandDriverResolveTrimsTrailingNewline(t *testing.T) {
	d := CommandDriver{Command: "echo hello"}
	got, err := d.Resolve(context.Background(), "k")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Reveal() != "hello" {
		t.Fatalf("Reveal() = %q, want %q", got.Reveal(), "hello")
	}
}

func TestCommandDriverResolveEmptyOutputErrors(t *testing.T) {
	d := CommandDriver{Command: "true"}
	if _, err := d.Resolve(context.Background(), "k"); err == nil {
		t.Fatal("Resolve: want error for empty output, got nil")
	}
}

func TestCommandDriverResolveCommandFailureErrorsWithStderr(t *testing.T) {
	d := CommandDriver{Command: "echo failmsg >&2; exit 1"}
	_, err := d.Resolve(context.Background(), "k")
	if err == nil {
		t.Fatal("Resolve: want error for nonzero exit, got nil")
	}
	if !strings.Contains(err.Error(), "failmsg") {
		t.Fatalf("Resolve error = %q, want it to contain stderr %q", err.Error(), "failmsg")
	}
}

func TestCommandDriverResolveFailureDoesNotLeakPartialStdout(t *testing.T) {
	d := CommandDriver{Command: "printf 'partial-secret-fragment'; echo failmsg >&2; exit 1"}
	_, err := d.Resolve(context.Background(), "k")
	if err == nil {
		t.Fatal("Resolve: want error for nonzero exit, got nil")
	}
	if strings.Contains(err.Error(), "partial-secret-fragment") {
		t.Fatalf("error %q leaks partially captured stdout", err.Error())
	}
}

func TestCommandDriverResolveEmptyKeyNameErrors(t *testing.T) {
	d := CommandDriver{Command: "echo hi"}
	if _, err := d.Resolve(context.Background(), ""); err == nil {
		t.Fatal("Resolve: want error for empty key name, got nil")
	}
}

func TestCommandDriverResolveTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	d := CommandDriver{Command: "sleep 5"}
	if _, err := d.Resolve(ctx, "k"); err == nil {
		t.Fatal("Resolve: want error for context deadline, got nil")
	}
}

func TestNewCommandDriverRequiresCommand(t *testing.T) {
	if _, err := New("command", map[string]any{}); err == nil {
		t.Fatal("New: want error for missing config.command, got nil")
	}
}

func TestNewCommandDriverResolvesThroughFactory(t *testing.T) {
	d, err := New("command", map[string]any{"command": "echo -n from-factory"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := d.Resolve(context.Background(), "k")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Reveal() != "from-factory" {
		t.Fatalf("Reveal() = %q, want %q", got.Reveal(), "from-factory")
	}
}
