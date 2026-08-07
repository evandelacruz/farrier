package keystore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// keyNamePlaceholder is substituted with the requested key name in
// CommandResolver's Args before the command runs, so one configured
// command can resolve any number of named keys (e.g. `op read
// op://vault/{{key}}`).
const keyNamePlaceholder = "{{key}}"

// keyNameEnvVar additionally carries the requested key name into the
// command's environment, for commands that read it there instead of (or
// in addition to) a placeholder argument.
const keyNameEnvVar = "FARRIER_KEY_NAME"

// CommandResolver is the "command" keystore driver (KEY-002): it resolves
// key material from the stdout of any operator-specified command — one
// interface that covers 1Password CLI, Vault, `pass`, sops, cloud secret
// managers, and anything else the team already uses (spec.md "Bundle
// config is shareable; keys resolve through drivers").
//
// The command's stdout is captured into memory and never forwarded to
// this process's own stdout or surfaced in any error — only the resolved
// Secret carries it onward. On failure, the error reports the command's
// stderr (a human-readable diagnostic channel, not key material) but
// never the stdout captured so far, which could hold a partially printed
// secret.
type CommandResolver struct {
	// Command is the executable to run. A bare name (no path separator)
	// resolves via PATH.
	Command string
	// Args are passed to Command. Any arg containing the literal
	// "{{key}}" has it replaced with the requested key name.
	Args []string
}

// NewCommand returns a CommandResolver that runs command with args.
func NewCommand(command string, args ...string) (*CommandResolver, error) {
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("keystore: command: command is required")
	}
	return &CommandResolver{Command: command, Args: args}, nil
}

// Resolve runs Command with Args (after key-name substitution) and returns
// its captured stdout, trailing newline trimmed, as a Secret.
func (r *CommandResolver) Resolve(ctx context.Context, keyName string) (Secret, error) {
	if strings.TrimSpace(keyName) == "" {
		return Secret{}, errors.New("keystore: command: key name is required")
	}

	args := make([]string, len(r.Args))
	for i, a := range r.Args {
		args[i] = strings.ReplaceAll(a, keyNamePlaceholder, keyName)
	}

	cmd := exec.CommandContext(ctx, r.Command, args...)
	cmd.Env = append(os.Environ(), keyNameEnvVar+"="+keyName)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Secret{}, fmt.Errorf("keystore: command: resolve %q: %w", keyName, ctx.Err())
		}
		return Secret{}, fmt.Errorf("keystore: command: resolve %q: %s: %w%s", keyName, r.Command, err, stderrSuffix(stderr.String()))
	}

	return NewSecret(strings.TrimRight(stdout.String(), "\r\n")), nil
}

func stderrSuffix(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return fmt.Sprintf(" (stderr: %s)", stderr)
}
