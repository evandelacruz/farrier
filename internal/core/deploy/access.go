package deploy

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/forge"
)

// What this file is for
//
// Forge state lives in host directories bind-mounted into the forgejo
// container (tech-spec.md "Host state layout"), and the container's
// processes run as forgeUID:forgeGID rather than as the operator. Something
// has to arrange for those directories to be usable by that uid — and the
// mechanism that arranges it is not the same everywhere.
//
// On a Linux host a bind mount passes real uids straight through, so the
// directories must actually be owned by forgeUID:forgeGID, and `chown` is
// how they get that way. On a macOS host the container runtime maps
// ownership at the mount boundary — the container sees the files as owned
// by whoever it runs as, whatever the host says — so the chown is both
// unnecessary and impossible: an ordinary user cannot give a file away to
// another uid, and the call fails with "Operation not permitted".
//
// Farrier does not detect which of those it is on. Doing so would put
// locality-dependent behavior back into `up` — ORCH-003's whole point is
// that `ssh://user@localhost` runs the identical path as a remote host, and
// a branch on the target's operating system, or on whether the target is
// local, is exactly the thing that stops being true. So instead of asking
// which mechanism applies, Up arranges what it can and then tests the
// property both mechanisms exist to produce: the forge, as the uid it
// really runs as, against the real bind-mounted paths, can read and write
// its state. A chown that could not be applied is not a failure; a forge
// that cannot use its state is.
//
// The probe runs inside the bundle's own pinned forgejo image, as
// forgeUID:forgeGID, with the host's state directories mounted at the
// container paths app.ini already names (forge.RepoRoot, forge.DataPath) —
// so what it exercises is the deployment's real access path rather than a
// model of it. Inspecting the mode bits or the owner would answer a
// different question, and answer it wrong on macOS, where the bits look
// wrong and the access works.
//
// Each caller verifies what it placed
//
// A probe covers the paths it is handed and nothing else, so the caller
// that created state is the one that checks it. `up` creates the two state
// directories and nothing beneath them, and verifies exactly those
// (verifyForgeCanUseState). `restore` writes a directory per repository
// under state/git and the database file under state/gitea, before `up`
// runs at all, and verifies those
// (VerifyForgeCanUsePlacedState) — it is the only caller that knows those
// paths, and it has them in hand rather than having to walk the host for
// them.
//
// Neither substitutes for the other. On a fresh host the two rise and fall
// together, because one `mkdir -p` creates a state directory and its new
// children with the same ownership. They come apart on a target whose
// state directories already exist and are already forge-owned from an
// earlier `up` — a restore re-run, a drill whose teardown did not finish,
// disaster recovery onto a previously-provisioned host. There a failed
// recursive chown leaves the top of each directory fine and everything
// restore just extracted owned by the SSH session's user, which the
// top-level probe passes straight over.

// accessProbeFile is the file the probe writes and removes inside each
// state directory. A dotfile so a probe that somehow outlives its own
// cleanup is inert to Forgejo, which enumerates repositories by directory.
const accessProbeFile = ".farrier-access-check"

// accessProbeContent is written and read back, rather than only written,
// so a directory that accepts a create but serves nothing back — an empty
// or broken mount — fails the probe instead of passing it.
const accessProbeContent = "farrier"

// verifyForgeCanUseState reports whether the forge can read and write both
// state directories on host, as the uid it runs as. On failure it returns
// an error naming the state directories and what the operator can do about
// it; the underlying error carries the probe's own stderr, which names the
// single directory that actually failed.
//
// It leaves nothing behind: the probe removes its file on the success path
// and on every failure path, including the read-back failing after the
// write succeeded.
func verifyForgeCanUseState(ctx context.Context, host Host, image, remoteDir string) error {
	script := stateProbeScript([]probeDir{
		{container: forge.RepoRoot, host: GitStatePath(remoteDir)},
		{container: forge.DataPath, host: GiteaStatePath(remoteDir)},
	})
	if err := runAsForge(ctx, host, image, remoteDir, script); err != nil {
		return fmt.Errorf(
			"the forge cannot read and write its state on this host: running as uid %d it was denied access under %s and %s: %w. "+
				"Run `up` as a user that can change those directories to owner %d:%d, or create them yourself owned by %d:%d before re-running",
			forgeUID, GitStatePath(remoteDir), GiteaStatePath(remoteDir), err,
			forgeUID, forgeGID, forgeUID, forgeGID)
	}
	return nil
}

// VerifyForgeCanUsePlacedState reports whether the forge can use state a
// caller placed on host itself — dirs it created under the state
// directories, and files it wrote there — as the uid the forge runs as.
// restore.placeState is the caller: it extracts a directory per repository
// under GitStatePath and writes the database under GiteaStatePath over an
// SSH session that generally isn't forgeUID:forgeGID, and its recursive
// chown is best-effort like every other one here. This is what turns that
// into an outcome the restore refuses to succeed past, and it is the only
// thing standing where the chown's own failure used to: without it, a
// restore onto an already-provisioned host reports success and leaves
// Forgejo unable to write the repositories it just restored.
//
// Every path must be under GitStatePath or GiteaStatePath — those are the
// two directories the probe mounts, and a path outside them is a caller
// bug rather than an access failure, so it is refused as such.
//
// It leaves nothing behind, on the success path and on a failure partway
// alike. Directories get the same write-read-remove probe file `up` uses.
// Files are opened for reading and for writing and then closed, without a
// byte going either way, so a database restore just placed comes out of
// the check exactly as it went in and there is nothing to clean up.
func VerifyForgeCanUsePlacedState(ctx context.Context, host Host, image, remoteDir string, dirs, files []string) error {
	probeDirs, err := probePaths(remoteDir, dirs)
	if err != nil {
		return err
	}
	probeFiles, err := probePaths(remoteDir, files)
	if err != nil {
		return err
	}

	script := stateProbeScript(probeDirs) + "; " + fileProbeScript(probeFiles)
	if err := runAsForge(ctx, host, image, remoteDir, script); err != nil {
		return fmt.Errorf(
			"the forge cannot read and write the state just placed on this host: running as uid %d it was denied access under %s: %w. "+
				"Re-run as a user that can change those paths to owner %d:%d, or clear %s on the target and restore onto an empty host",
			forgeUID, summarizePaths(slices.Concat(dirs, files)), err,
			forgeUID, forgeGID, path.Join(remoteDir, stateDir))
	}
	return nil
}

// probePaths pairs each host path with the path the probe sees for it
// inside the container.
func probePaths(remoteDir string, hostPaths []string) ([]probeDir, error) {
	out := make([]probeDir, 0, len(hostPaths))
	for _, p := range hostPaths {
		container, err := containerPath(remoteDir, p)
		if err != nil {
			return nil, err
		}
		out = append(out, probeDir{container: container, host: p})
	}
	return out, nil
}

// containerPath maps a host path under GitStatePath or GiteaStatePath to
// the path the probe sees for it inside the container — the inverse of the
// two bind mounts runAsForge sets up, kept next to them so the two cannot
// drift apart.
func containerPath(remoteDir, hostPath string) (string, error) {
	for _, m := range []probeDir{
		{host: GitStatePath(remoteDir), container: forge.RepoRoot},
		{host: GiteaStatePath(remoteDir), container: forge.DataPath},
	} {
		if hostPath == m.host {
			return m.container, nil
		}
		if rest, ok := strings.CutPrefix(hostPath, m.host+"/"); ok {
			return m.container + "/" + rest, nil
		}
	}
	return "", fmt.Errorf("cannot check %s: it is not under %s or %s", hostPath, GitStatePath(remoteDir), GiteaStatePath(remoteDir))
}

// summarizePaths renders paths for a failure message: all of them when a
// restore placed few, and the first few plus a count when it placed more
// than an error line can usefully carry. The probe's own stderr, which the
// wrapped error carries, names the single path that actually failed; this
// says what was covered.
func summarizePaths(paths []string) string {
	const shown = 3
	if len(paths) <= shown {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:shown], ", "), len(paths)-shown)
}

// verifyForgeCanReadSSHHostKey reports whether the forge can read the SSH
// host key configureSSHHostKey just shipped, as the uid it runs as. The key
// is written 0600 by an SSH session that generally isn't that uid, so
// whether the container can read it turns on the same arrangement — real
// ownership, or ownership mapped at the mount — that the state directories
// do. Without this the failure surfaces much later and much worse: Forgejo
// finds no readable host key and generates an unmanaged one, and every
// client that knew this instance sees a changed host identity.
func verifyForgeCanReadSSHHostKey(ctx context.Context, host Host, image, remoteDir string) error {
	hostPath := path.Join(GiteaStatePath(remoteDir), sshHostKeyRelPath())
	script := readProbeScript(probeDir{container: forge.SSHHostKeyPath, host: hostPath})
	if err := runAsForge(ctx, host, image, remoteDir, script); err != nil {
		return fmt.Errorf(
			"the forge cannot read its ssh host key: running as uid %d it was denied %s: %w. "+
				"Run `up` as a user that can change the state directories to owner %d:%d, or create them yourself owned by %d:%d before re-running",
			forgeUID, hostPath, err, forgeUID, forgeGID, forgeUID, forgeGID)
	}
	return nil
}

// probeDir pairs a path as the probe sees it, inside the container, with
// the same path as the operator sees it, on the host. The container path is
// what the probe acts on; the host path is what it names when it fails,
// because an operator handed a container path has been told where the
// problem is in a namespace they cannot go fix it in.
type probeDir struct {
	container string
	host      string
}

// stateProbeScript builds the /bin/sh program that writes, reads back, and
// removes a probe file in each of dirs, printing the host path of the first
// directory that fails to stderr and exiting non-zero. Built here rather
// than inline so a test can run the real script against real directories
// and hold it to what it claims: that it detects a directory it cannot use,
// and that it leaves nothing behind whether it succeeds or fails.
func stateProbeScript(dirs []probeDir) string {
	// The cleanup before the write matters as much as the ones after it: a
	// probe file left by an interrupted earlier run would otherwise let a
	// failed write read back the old content and pass.
	script := fmt.Sprintf(
		// The write and the read-back are grouped under one 2>/dev/null so
		// that a redirection the shell itself refuses is quiet too: its
		// complaint would otherwise reach stderr ahead of the redirection
		// meant to silence it, and stderr is what names the failing
		// directory.
		"probe() { f=\"$1/%[1]s\"; rm -f \"$f\" 2>/dev/null; "+
			"{ printf %[2]s > \"$f\" && [ \"$(cat \"$f\")\" = %[2]s ]; } 2>/dev/null || "+
			"{ rm -f \"$f\" 2>/dev/null; echo \"$2\" >&2; exit 1; }; rm -f \"$f\" 2>/dev/null; }",
		accessProbeFile, accessProbeContent,
	)
	for _, d := range dirs {
		script += fmt.Sprintf("; probe %s %s", stateShQuote(d.container), stateShQuote(d.host))
	}
	return script
}

// fileProbeScript builds the /bin/sh program that checks each of files is
// there and can be opened for reading and for writing, printing the host
// path of the first one that fails to stderr and exiting non-zero.
//
// It opens and closes, rather than reading or writing bytes: opening is
// where permission is decided, and a file restore just placed — the
// database above all — must come out of a check byte-for-byte what it went
// in as. The existence test in front of the append matters for the same
// reason: without it a database that never arrived would be created empty
// here and pass.
//
// `true` rather than `:` because `:` is a POSIX special built-in, and a
// redirection error on one of those exits a non-interactive shell outright
// — before the handler that names the failing path could run.
func fileProbeScript(files []probeDir) string {
	// Each redirection is grouped under its own 2>/dev/null so the shell's
	// own complaint about the redirection it refused is silenced too:
	// stderr is what names the failing path, and nothing else may reach it.
	script := "probefile() { [ -f \"$1\" ] && { true < \"$1\"; } 2>/dev/null && { true >> \"$1\"; } 2>/dev/null || " +
		"{ echo \"$2\" >&2; exit 1; }; }"
	for _, f := range files {
		script += fmt.Sprintf("; probefile %s %s", stateShQuote(f.container), stateShQuote(f.host))
	}
	return script
}

// readProbeScript builds the /bin/sh program that reads file, printing its
// host path to stderr and exiting non-zero if it cannot. It writes nothing,
// so there is nothing to clean up.
func readProbeScript(file probeDir) string {
	return fmt.Sprintf(
		"cat %s > /dev/null 2>&1 || { echo %s >&2; exit 1; }",
		stateShQuote(file.container), stateShQuote(file.host),
	)
}

// runAsForge runs script inside the bundle's pinned forgejo image as
// forgeUID:forgeGID, with the host's state directories bind-mounted at the
// same container paths the deployment itself mounts them at — the whole
// point being that the probe travels the deployment's real access path.
//
// The container gets no network: it reads and writes two directories and
// needs nothing else, and a rehearsal instance must not be able to reach
// the outside world (DRIL-002). On a first `up` this is where the forgejo
// image is pulled, a moment earlier than Converge would have pulled it.
func runAsForge(ctx context.Context, host Host, image, remoteDir, script string) error {
	command := fmt.Sprintf(
		"docker run --rm --network none --entrypoint /bin/sh -u %d:%d -v %s:%s -v %s:%s %s -c %s",
		forgeUID, forgeGID,
		stateShQuote(GitStatePath(remoteDir)), forge.RepoRoot,
		stateShQuote(GiteaStatePath(remoteDir)), forge.DataPath,
		stateShQuote(image),
		stateShQuote(script),
	)
	_, err := host.Output(ctx, command)
	return err
}

// chownBestEffort asks host to give dirs to forgeUID:forgeGID and reports
// whether that worked. It never returns an error: on a host whose container
// runtime maps ownership across the mount boundary, the call cannot succeed
// and does not need to, and the caller's own verification is what decides
// whether the deployment can proceed. recursive covers content that is
// already in place — restored state, a shipped key — rather than a
// directory the forge will populate itself.
func chownBestEffort(ctx context.Context, host Host, recursive bool, dirs ...string) bool {
	flag := ""
	if recursive {
		flag = "-R "
	}
	quoted := ""
	for _, d := range dirs {
		quoted += " " + stateShQuote(d)
	}
	_, err := host.Output(ctx, fmt.Sprintf("chown %s%d:%d%s", flag, forgeUID, forgeGID, quoted))
	return err == nil
}
