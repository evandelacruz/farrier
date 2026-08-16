package orchestrate

import (
	"fmt"
	"strings"
)

// DefaultRemoteDir is the host directory Farrier deploys into when the
// operator names none: Compose files, the rendered forge and Caddy config,
// and forge state all live under it.
//
// It lives here, next to the transport that creates it, so the flag
// default every command defines, the API's default for a request that
// omits it, and the advice remoteDirHint gives when the host refuses to
// create it cannot drift apart. A hint naming a default the CLI no longer
// uses would be worse than no hint.
const DefaultRemoteDir = "/opt/farrier"

// remoteDirHint returns the operator-facing explanation to append to a
// failed command's error when the host refused to create or write a path.
// It is empty for every other failure.
//
// Without it the operator gets `mkdir: cannot create directory
// '/opt/farrier': Permission denied` and nothing else. That is true and
// nearly useless: it leaves out that Farrier chose that directory on their
// behalf, that an ordinary SSH user cannot create anything there, and that
// they can name a directory they already own instead. The default suits a
// dedicated host reached as root and suits nothing else, so this is the
// first wall an operator deploying to their own machine walks into.
func remoteDirHint(command, stderr string) string {
	if !writesPath(command) || !refusedAccess(stderr) {
		return ""
	}
	return fmt.Sprintf(": the host refused to create or write that path. Farrier deploys into %s unless told otherwise,"+
		" and on most hosts that directory belongs to root — an ordinary SSH user cannot create it. Either create it on the host"+
		" and give it to the user in your target, or pass -remote-dir an absolute path that user can already write, such as"+
		" /home/you/farrier (on macOS, keep it under /Users so Docker can share it). Whichever you choose, pass the same"+
		" -remote-dir to every later command against this host", DefaultRemoteDir)
}

// pathWritingCommands are the fragments that mark a command as creating or
// writing a path on the host, rather than merely running something there.
// They are the whole vocabulary Farrier uses to put content on a host:
// `mkdir -p` for the deploy directory and forge state, `cat >` for a
// shipped file, `touch` for the app.ini mountpoint, and `tar -C` for a
// restored repository.
var pathWritingCommands = []string{"mkdir ", "touch ", "cat >", "tar -C "}

// writesPath reports whether command puts something on the host's
// filesystem. It is what keeps the hint off a refusal that has nothing to
// do with the deploy directory — the Docker daemon socket above all, which
// an operator outside the `docker` group is denied on every host and which
// -remote-dir would do nothing about.
func writesPath(command string) bool {
	for _, fragment := range pathWritingCommands {
		if strings.Contains(command, fragment) {
			return true
		}
	}
	return false
}

// refusedAccess reports whether stderr is the host refusing a filesystem
// operation outright, as opposed to failing it for some other reason (a
// full disk, a path that is a file where a directory belongs). Those get
// no hint: they are real and different problems, and pointing their
// diagnosis at -remote-dir would send the operator somewhere useless.
//
// Matching on the message is what there is to match on — a remote command
// reports one exit status for every kind of failure, and the reason only
// ever comes back as the text `mkdir` or the shell wrote to stderr. The
// three phrases below are what a POSIX system produces for EACCES, EPERM,
// and EROFS.
func refusedAccess(stderr string) bool {
	lower := strings.ToLower(stderr)
	for _, phrase := range []string{"permission denied", "operation not permitted", "read-only file system"} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}
