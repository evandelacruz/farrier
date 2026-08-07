package orchestrate

import (
	"fmt"
	"strings"
)

// shQuote wraps s in single quotes for safe use as one argument in a
// /bin/sh command line, escaping any single quotes it contains. Every path
// this package interpolates into a remote command goes through it: remote
// directories come from a manifest, and an unquoted path with a space or a
// shell metacharacter would otherwise split into several arguments or run
// something the caller never wrote.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// stderrSuffix renders captured stderr as a parenthetical for an error
// message, or the empty string when there is nothing to report. A remote
// command that fails carries its diagnosis in stderr; without this the
// caller sees only an exit status.
func stderrSuffix(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return fmt.Sprintf(" (stderr: %s)", stderr)
}
