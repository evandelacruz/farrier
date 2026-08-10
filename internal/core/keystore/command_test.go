package keystore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

func TestCommandDriverResolveEmptyOutputIsNotFound(t *testing.T) {
	d := CommandDriver{Command: "true"}
	_, err := d.Resolve(context.Background(), "k")
	if err == nil {
		t.Fatal("Resolve: want error for empty output, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve error = %v, want it to wrap ErrNotFound so the rotation guard can tell absence from failure", err)
	}
}

func TestCommandDriverResolveFailureIsNotNotFound(t *testing.T) {
	d := CommandDriver{Command: "echo unauthenticated >&2; exit 1"}
	_, err := d.Resolve(context.Background(), "k")
	if err == nil {
		t.Fatal("Resolve: want error for nonzero exit, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("Resolve: a failing command must never read as an absent key")
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

// vaultCommands fakes an operator's secret manager with a directory: the
// resolve command prints the file named for the key if it exists and
// prints nothing (exiting zero) if it does not, which is exactly the
// "absent" signal the driver reads as ErrNotFound; the store command
// writes whatever it is handed on stdin.
func vaultCommands(t *testing.T) (dir, resolve, store string) {
	t.Helper()
	dir = t.TempDir()
	resolve = fmt.Sprintf(`f=%s/"$FARRIER_KEY_NAME"; if [ -f "$f" ]; then cat "$f"; fi`, dir)
	store = fmt.Sprintf(`cat > %s/"$FARRIER_KEY_NAME"`, dir)
	return dir, resolve, store
}

func TestWritableCommandDriverStoreRoundTrips(t *testing.T) {
	_, resolve, store := vaultCommands(t)
	d := WritableCommandDriver{CommandDriver: CommandDriver{Command: resolve}, StoreCommand: store}

	if err := d.Store(context.Background(), "forgejo_secret_key", NewSecret("minted-value")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := d.Resolve(context.Background(), "forgejo_secret_key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Reveal() != "minted-value" {
		t.Fatalf("Reveal() = %q, want %q", got.Reveal(), "minted-value")
	}
}

func TestWritableCommandDriverStoreUsesKeyNameEnvVar(t *testing.T) {
	dir, resolve, store := vaultCommands(t)
	d := WritableCommandDriver{CommandDriver: CommandDriver{Command: resolve}, StoreCommand: store}

	if err := d.Store(context.Background(), "lfs_jwt_secret", NewSecret("v")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lfs_jwt_secret")); err != nil {
		t.Fatalf("store command did not see FARRIER_KEY_NAME: %v", err)
	}
}

// The secret travels on stdin and nowhere else (KEY-003): not in the
// command's environment, and not in its argv.
func TestWritableCommandDriverStoreKeepsSecretOffEnvAndArgv(t *testing.T) {
	dir := t.TempDir()
	envDump := filepath.Join(dir, "env")
	argvDump := filepath.Join(dir, "argv")
	command := fmt.Sprintf(`env > %s; cat /proc/$$/cmdline > %s 2>/dev/null; cat > /dev/null`, envDump, argvDump)

	d := WritableCommandDriver{CommandDriver: CommandDriver{Command: "true"}, StoreCommand: command}
	if err := d.Store(context.Background(), "forgejo_secret_key", NewSecret("super-secret-value")); err != nil {
		t.Fatalf("Store: %v", err)
	}

	dumps := []string{envDump}
	if runtime.GOOS == "linux" {
		dumps = append(dumps, argvDump)
	}
	for _, path := range dumps {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(data), "super-secret-value") {
			t.Fatalf("%s carries the secret; it must travel on stdin only", filepath.Base(path))
		}
	}
}

func TestWritableCommandDriverStoreFailureErrorsWithStderr(t *testing.T) {
	d := WritableCommandDriver{
		CommandDriver: CommandDriver{Command: "true"},
		StoreCommand:  "cat > /dev/null; echo 'vault session expired' >&2; exit 1",
	}
	err := d.Store(context.Background(), "k", NewSecret("aG7xQ2minted-value"))
	if err == nil {
		t.Fatal("Store: want error for nonzero exit, got nil")
	}
	if !strings.Contains(err.Error(), "vault session expired") {
		t.Fatalf("Store error = %q, want it to carry the command's stderr", err.Error())
	}
}

func TestWritableCommandDriverStoreFailureRedactsSecretFromStderr(t *testing.T) {
	d := WritableCommandDriver{
		CommandDriver: CommandDriver{Command: "true"},
		StoreCommand:  "cat >&2; exit 1",
	}
	err := d.Store(context.Background(), "k", NewSecret("super-secret-value"))
	if err == nil {
		t.Fatal("Store: want error for nonzero exit, got nil")
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("Store error %q echoes the secret back through stderr", err.Error())
	}
}

func TestWritableCommandDriverStoreDiscardsStdout(t *testing.T) {
	d := WritableCommandDriver{
		CommandDriver: CommandDriver{Command: "true"},
		StoreCommand:  "cat; exit 1",
	}
	err := d.Store(context.Background(), "k", NewSecret("super-secret-value"))
	if err == nil {
		t.Fatal("Store: want error for nonzero exit, got nil")
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("Store error %q carries the command's stdout", err.Error())
	}
}

func TestWritableCommandDriverStoreEmptyKeyNameErrors(t *testing.T) {
	d := WritableCommandDriver{CommandDriver: CommandDriver{Command: "true"}, StoreCommand: "cat > /dev/null"}
	if err := d.Store(context.Background(), "", NewSecret("v")); err == nil {
		t.Fatal("Store: want error for empty key name, got nil")
	}
}

func TestWritableCommandDriverStoreIgnoresUnreadStdin(t *testing.T) {
	d := WritableCommandDriver{CommandDriver: CommandDriver{Command: "true"}, StoreCommand: "exit 0"}
	if err := d.Store(context.Background(), "k", NewSecret("v")); err != nil {
		t.Fatalf("Store: %v", err)
	}
}

func TestWritableCommandDriverStoreTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	d := WritableCommandDriver{CommandDriver: CommandDriver{Command: "true"}, StoreCommand: "sleep 5"}
	if err := d.Store(ctx, "k", NewSecret("v")); err == nil {
		t.Fatal("Store: want error for context deadline, got nil")
	}
}

func TestNewCommandDriverWithoutStoreCommandIsResolveOnly(t *testing.T) {
	d, err := New("command", map[string]any{"command": "echo hi"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := d.(Writer); ok {
		t.Fatal("New: a command driver without storeCommand must not implement Writer, so init rejects it at validate")
	}
}

func TestNewCommandDriverWithStoreCommandWrites(t *testing.T) {
	dir, resolve, store := vaultCommands(t)
	d, err := New("command", map[string]any{"command": resolve, "storeCommand": store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w, ok := d.(Writer)
	if !ok {
		t.Fatal("New: a command driver with storeCommand must implement Writer")
	}
	if err := w.Store(context.Background(), "forgejo_secret_key", NewSecret("minted")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "forgejo_secret_key"))
	if err != nil {
		t.Fatalf("read stored key: %v", err)
	}
	if string(data) != "minted" {
		t.Fatalf("stored %q, want %q", string(data), "minted")
	}
}

// The rotation guard wraps the command driver like every other Writer, so
// a second store of non-rotating key material is refused — and the first
// one, against an empty vault, still goes through.
func TestNewCommandDriverStoreGoesThroughRotationGuard(t *testing.T) {
	_, resolve, store := vaultCommands(t)
	d, err := New("command", map[string]any{"command": resolve, "storeCommand": store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := d.(Writer)

	if err := w.Store(context.Background(), "forgejo_secret_key", NewSecret("first")); err != nil {
		t.Fatalf("Store into an empty keystore: %v", err)
	}
	if err := w.Store(context.Background(), "forgejo_secret_key", NewSecret("second")); err == nil {
		t.Fatal("Store: want refusal overwriting non-rotating key material, got nil")
	}
	if err := w.Store(context.Background(), KeyTLSCertificate, NewSecret("cert-1")); err != nil {
		t.Fatalf("Store rotating key material: %v", err)
	}
	if err := w.Store(context.Background(), KeyTLSCertificate, NewSecret("cert-2")); err != nil {
		t.Fatalf("Store rotating key material a second time: %v", err)
	}
}

func TestNewCommandDriverRejectsBlankStoreCommand(t *testing.T) {
	if _, err := New("command", map[string]any{"command": "echo hi", "storeCommand": "  "}); err == nil {
		t.Fatal("New: want error for a present but blank storeCommand, got nil")
	}
	if _, err := New("command", map[string]any{"command": "echo hi", "storeCommand": 7}); err == nil {
		t.Fatal("New: want error for a non-string storeCommand, got nil")
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
