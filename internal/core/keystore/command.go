package keystore

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
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
		return Secret{}, fmt.Errorf("keystore: command: key %q produced no output", keyName)
	}
	return NewSecret(string(secret)), nil
}

func newCommandDriver(config map[string]any) (Driver, error) {
	command, err := stringConfig(config, "command")
	if err != nil {
		return nil, fmt.Errorf("keystore: command: %w", err)
	}
	return CommandDriver{Command: command}, nil
}

func stderrSuffix(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return fmt.Sprintf(" (stderr: %s)", stderr)
}
