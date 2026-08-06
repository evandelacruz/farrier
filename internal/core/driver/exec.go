package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Invoker calls a single named driver operation with JSON-encodable params
// and decodes its JSON result into result (a pointer), or returns an
// error. A nil result discards the operation's return value.
//
// This is the seam every driver-type package is built on: an in-tree
// driver implements that package's own domain interface directly, while an
// out-of-tree driver is reached through an Invoker — normally an Exec —
// that the domain package's own adapter wraps.
type Invoker interface {
	Invoke(ctx context.Context, method string, params, result any) error
}

// Exec is an Invoker backed by a standalone executable speaking the exec
// protocol: one Request as JSON on stdin, one Response as JSON on stdout,
// per call. It starts the process fresh for every Invoke — the protocol is
// one call per process, not a long-lived session — so a driver executable
// needs no state beyond what params and its own configuration give it.
type Exec struct {
	// Path is the executable to run. It is passed to exec.CommandContext
	// as-is, so a bare name (no path separator) resolves via PATH.
	Path string
	// Args are fixed arguments passed before any protocol data. The
	// request itself always arrives on stdin, never as an argument, so
	// it never appears in a process listing.
	Args []string
	// Timeout bounds one Invoke call, including process startup. Zero
	// means no bound beyond ctx's own deadline, if any.
	Timeout time.Duration
}

// Invoke runs the configured executable once, writes method and params to
// its stdin as a Request, and decodes its stdout as a Response.
//
// A nonzero exit code, stdout that fails to parse as a Response, or a
// Response with OK false all produce an error naming the driver, the
// method, and the executable's stderr — stderr is surfaced because it is
// the only channel a third-party executable has for a human-readable
// diagnostic, without this package prescribing its format. Invoke never
// forwards the child process's stdin/stdout/stderr anywhere on its own;
// whether a caller exposes that as job progress or a log line is the
// caller's decision, not this package's.
func (e Exec) Invoke(ctx context.Context, method string, params, result any) error {
	if e.Path == "" {
		return errors.New("driver: exec: no executable path configured")
	}

	var rawParams json.RawMessage
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("driver: exec %s: encode params: %w", method, err)
		}
		rawParams = encoded
	}
	reqBody, err := json.Marshal(Request{Method: method, Params: rawParams})
	if err != nil {
		return fmt.Errorf("driver: exec %s: encode request: %w", method, err)
	}

	if e.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, e.Path, e.Args...)
	cmd.Stdin = bytes.NewReader(reqBody)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if runErr := cmd.Run(); runErr != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("driver: exec %s: %s: %w", method, e.Path, ctx.Err())
		}
		return fmt.Errorf("driver: exec %s: %s: %w%s", method, e.Path, runErr, stderrSuffix(stderr.String()))
	}

	var resp Response
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
		return fmt.Errorf("driver: exec %s: %s: invalid response: %w%s", method, e.Path, err, stderrSuffix(stderr.String()))
	}
	if !resp.OK {
		msg := resp.Error
		if msg == "" {
			msg = "driver reported failure with no error message"
		}
		return fmt.Errorf("driver: exec %s: %s: %s", method, e.Path, msg)
	}

	if result == nil || len(resp.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		return fmt.Errorf("driver: exec %s: %s: decode result: %w", method, e.Path, err)
	}
	return nil
}

func stderrSuffix(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return fmt.Sprintf(" (stderr: %s)", stderr)
}
