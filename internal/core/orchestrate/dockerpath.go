package orchestrate

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// dockerSearchPath is the bounded, explicit list of directories Farrier will
// look in for `docker` when the session's own PATH does not have it. It is
// the whole of the fallback: no wildcards, no walking, no probing anything
// not named here.
//
// Each entry earns its place by being where a stock install of Docker puts
// the binary on a platform Farrier targets:
//
//   - $HOME/.docker/bin — Docker Desktop's per-user install (the macOS
//     default). Docker Desktop puts this on PATH by editing the shell's
//     interactive rc file, which is exactly the file an SSH command session
//     does not read. This entry is the defect this list exists to fix.
//   - /usr/local/bin — Docker Desktop's system-wide install, Homebrew on
//     Intel macOS, and hand-installed binaries.
//   - /opt/homebrew/bin — Homebrew on Apple Silicon.
//   - /snap/bin — Ubuntu's snap-packaged Docker.
//
// Linux package installs land in /usr/bin, which is on every login and
// non-login PATH already, so they need no entry.
var dockerSearchPath = []string{
	"$HOME/.docker/bin",
	"/usr/local/bin",
	"/opt/homebrew/bin",
	"/snap/bin",
}

// dockerPathPreamble is prepended to every command the transport runs. When
// `docker` already resolves it does nothing at all — no assignment, no extra
// round trip, and the command that follows runs against the PATH the host
// gave the session. Only when `docker` is absent does it append
// dockerSearchPath, so the fallback can never shadow a tool the host already
// has.
//
// It rides along inside the session that was going to run anyway, which is
// what makes the fix free in the common case. Resolving the location from
// the control plane instead — a `command -v docker` probe, or asking for a
// login shell — would cost a round trip per connection whether or not
// anything was wrong, and would still be wrong for a host that gains Docker
// mid-session.
//
// PATH is already exported in any session sshd starts, so assigning to it
// keeps the export and every command in the rest of the string, including
// anything nested in `sh -c`, inherits it.
var dockerPathPreamble = fmt.Sprintf(
	"command -v docker >/dev/null 2>&1 || PATH=\"$PATH:%s\"\n",
	strings.Join(dockerSearchPath, ":"),
)

// withDockerPath returns command with the PATH fallback ahead of it. Errors
// report the original command, not this, so an operator reading a failure
// sees what Farrier meant to run.
func withDockerPath(command string) string {
	return dockerPathPreamble + command
}

// invokesDocker reports whether command actually runs docker, as opposed to
// merely naming a path that contains the word (`compose/docker-compose.yml`
// is shipped by WriteFile on every converge).
func invokesDocker(command string) bool {
	return strings.Contains(command, "docker ")
}

// dockerMissingHint returns the operator-facing explanation to append to a
// failed command's error when the failure is `docker` not being found —
// exit status 127 from a command that runs docker. It is empty for every
// other failure.
//
// Without it the operator gets `command not found: docker` and nothing else,
// which is actively misleading on a machine where docker works fine in a
// terminal. The hint names the reason, what was already searched, and the
// one-line repair.
func dockerMissingHint(command string, err error) string {
	if !invokesDocker(command) || !isCommandNotFound(err) {
		return ""
	}
	return fmt.Sprintf(": docker was not found on this host. An SSH command session is"+
		" non-interactive and non-login, so it reads neither .zshrc nor .bash_profile —"+
		" a docker that works in your terminal can still be invisible here. Farrier also"+
		" looked in %s. If docker lives somewhere else, put it on the non-interactive PATH"+
		" (on macOS, add it to ~/.zshenv) or link it into /usr/local/bin",
		strings.Join(dockerSearchPath, ", "))
}

// isCommandNotFound reports whether err is a remote exit status 127, the
// status every POSIX shell uses for a command it could not find.
func isCommandNotFound(err error) bool {
	var exitErr *ssh.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitStatus() == 127
}
