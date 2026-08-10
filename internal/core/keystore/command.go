package keystore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

var (
	_ Driver = CommandDriver{}
	_ Writer = WritableCommandDriver{}
)

// waitDelay bounds how long Resolve waits for stdout/stderr to close after
// the command exits or is killed. Without it, a configured command that
// forks a grandchild (a pipeline, a backgrounded helper) can hold the
// output pipe open indefinitely after the shell itself is gone, and
// context cancellation would have no effect on when Resolve returns.
const waitDelay = 2 * time.Second

// keyNameEnvVar carries the keyName being resolved into the configured
// command's environment, so one command can resolve every key the bundle
// needs by branching on it.
const keyNameEnvVar = "FARRIER_KEY_NAME"

// CommandDriver resolves key material from the stdout of an
// operator-specified command (KEY-002) — one interface that covers
// 1Password CLI, Vault, pass, sops, cloud secret managers, and anything
// else the team already uses. Config: {"command": "<shell command>"}.
//
// It is the resolve-only half of the driver: configured without a
// storeCommand, `command` deliberately does not implement Writer, so
// initialize.Run's type assertion fails at StepValidate rather than after
// an ACME round trip. WritableCommandDriver is what New builds when
// config.storeCommand is present.
type CommandDriver struct {
	Command string
}

// Resolve runs Command through the shell with keyName in FARRIER_KEY_NAME
// and returns its stdout, minus a single trailing newline, as a Secret —
// the same trimming shell command substitution applies, so operators can
// write ordinary `echo`/CLI-tool commands without worrying about a stray
// newline corrupting the secret. Captured stdout is wrapped into a Secret
// immediately and never appears in an error message; only stderr does,
// for diagnostics (KEY-003).
//
// A command that exits zero having printed nothing is the driver's
// positive "not found": the command ran to completion and reported no
// value, so Resolve wraps ErrNotFound. That distinction is load-bearing
// rather than cosmetic — guardedDriver.Store treats only ErrNotFound as
// "safe to write" (see keystore.go), so without it the guard would refuse
// every store of freshly minted non-rotating key material and init could
// never write through this driver at all. A non-zero exit stays a hard
// failure: a secret manager that is unreachable, unauthenticated, or
// simply broken must never read as an empty slot.
func (d CommandDriver) Resolve(ctx context.Context, keyName string) (Secret, error) {
	if strings.TrimSpace(keyName) == "" {
		return Secret{}, fmt.Errorf("keystore: command: key name is required")
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", d.Command)
	cmd.Env = append(os.Environ(), keyNameEnvVar+"="+keyName)
	cmd.WaitDelay = waitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Secret{}, fmt.Errorf("keystore: command: resolve key %q: %w", keyName, ctx.Err())
		}
		return Secret{}, fmt.Errorf("keystore: command: resolve key %q: %w%s", keyName, err, stderrSuffix(stderr.String()))
	}

	secret := bytes.TrimRight(stdout.Bytes(), "\r\n")
	if len(secret) == 0 {
		return Secret{}, fmt.Errorf("keystore: command: key %q not found: the resolve command exited zero with no output: %w", keyName, ErrNotFound)
	}
	return NewSecret(string(secret)), nil
}

// WritableCommandDriver is CommandDriver plus the write side KEY-002 asks
// for: a second operator-specified command that takes minted key material
// on stdin, so init can put a new instance's secrets straight into the
// team's own secret manager instead of demanding a file keystore and
// leaving a plaintext copy on disk. Config: {"command": "<shell command>",
// "storeCommand": "<shell command>"}.
//
// Whether this type or the resolve-only CommandDriver gets built is
// decided from config alone, before either exists — see newCommandDriver.
type WritableCommandDriver struct {
	CommandDriver
	StoreCommand string
}

// Store runs StoreCommand through the shell with keyName in
// FARRIER_KEY_NAME and the key material on stdin, and treats a zero exit
// as stored. Stdin is the only channel the secret travels on: never argv,
// never the environment, never a log line (KEY-003). The command's own
// stdout is discarded for the same reason — an operator's CLI that echoes
// what it just stored must not put it anywhere Farrier might surface it.
//
// A non-zero exit surfaces the command's stderr so the operator can tell
// an expired login from a missing vault, with any occurrence of the secret
// itself replaced by the same "[redacted]" Secret's own formatting uses:
// stderr is arbitrary text from a command Farrier does not control, and it
// ends up in an error that reaches the event stream.
func (d WritableCommandDriver) Store(ctx context.Context, keyName string, secret Secret) error {
	if strings.TrimSpace(keyName) == "" {
		return fmt.Errorf("keystore: command: key name is required")
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", d.StoreCommand)
	cmd.Env = append(os.Environ(), keyNameEnvVar+"="+keyName)
	cmd.WaitDelay = waitDelay
	cmd.Stdin = strings.NewReader(secret.Reveal())
	cmd.Stdout = io.Discard
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("keystore: command: store key %q: %w", keyName, ctx.Err())
		}
		return fmt.Errorf("keystore: command: store key %q: %w%s", keyName, err, stderrSuffix(redactSecret(stderr.String(), secret)))
	}
	return nil
}

// newCommandDriver decides the driver's store capability from config and
// returns a type that either implements Writer or does not — never one
// that implements it and fails at call time. init type-asserts Writer at
// StepValidate (KEY-004), so a keystore that cannot accept key material is
// rejected before zone control is proven and before anything is generated;
// deferring that answer to the first Store call would move the failure
// past an ACME round trip and past key generation.
func newCommandDriver(config map[string]any) (Driver, error) {
	command, err := stringConfig(config, "command")
	if err != nil {
		return nil, fmt.Errorf("keystore: command: %w", err)
	}
	resolver := CommandDriver{Command: command}

	storeCommand, ok, err := optionalStringConfig(config, "storeCommand")
	if err != nil {
		return nil, fmt.Errorf("keystore: command: %w", err)
	}
	if !ok {
		return resolver, nil
	}
	return WritableCommandDriver{CommandDriver: resolver, StoreCommand: storeCommand}, nil
}

// redactSecret removes key material from text a failing command wrote to
// stderr, so surfacing that stderr for diagnosis cannot leak the very
// value being stored (KEY-003). The match is a plain substring one, which
// can over-redact a trivially short value — the safe direction, and not a
// case that arises for the high-entropy material init generates.
func redactSecret(text string, secret Secret) string {
	value := secret.Reveal()
	if value == "" {
		return text
	}
	return strings.ReplaceAll(text, value, redacted)
}

func stderrSuffix(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return fmt.Sprintf(" (stderr: %s)", stderr)
}
