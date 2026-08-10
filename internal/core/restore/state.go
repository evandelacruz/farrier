package restore

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
)

// gitPair is one repository's two captured archives (capture.go's
// captureOneRepo and captureOneRepoRefs): the full, immutable object store,
// and the mutable ref state pinned to the moment BKUP-002's push hold was
// held.
type gitPair struct {
	objectsPath string
	refsPath    string
}

// placeState writes the snapshot's git data and database directly onto the
// host directories UP-004 pins forge state to (deploy.GitStatePath,
// deploy.GiteaStatePath) — before deploy.Up (called afterward, by
// runDeploy) ever starts the forgejo container that reads them — then
// chowns everything it just wrote to the uid:gid that container runs as
// (deploy.ChownState): the SSH session placing this content is generally
// not that user, and configureState's own chown (part of deploy.Up, run
// after this) only ever touches each directory's top level, which is
// enough for `up` but not for content restore populates itself.
//
// That chown is best-effort — it cannot be applied at all on a host whose
// container runtime maps ownership across the mount boundary — so placing
// state is not finished until the outcome it exists for is checked. The
// last thing this step does is run the forge, as the uid it really runs
// as, against the very paths it just wrote: each restored repository
// directory and the database file. A restore whose content the forge
// cannot use fails here, loudly, naming the path; it does not converge a
// host and report a success Forgejo will discover is a lie on its first
// write.
func placeState(ctx context.Context, job *events.Job, plainDir string, manifest *backup.Manifest, opts Options) error {
	job.Started(StepPlaceState, "restoring git repositories and database onto host")

	pairs, names, err := gitComponentPairs(manifest)
	if err != nil {
		job.Emit(StepPlaceState, events.StateFailed, err.Error())
		return err
	}

	gitRoot := deploy.GitStatePath(opts.RemoteDir)
	repoDirs := make([]string, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			job.Emit(StepPlaceState, events.StateFailed, err.Error())
			return err
		}
		if err := placeOneRepo(ctx, opts.Host, plainDir, gitRoot, name, pairs[name]); err != nil {
			job.Emit(StepPlaceState, events.StateFailed, err.Error())
			return err
		}
		repoDirs = append(repoDirs, repoDirPath(gitRoot, name))
	}

	dbComponent, err := databaseComponent(manifest)
	if err != nil {
		job.Emit(StepPlaceState, events.StateFailed, err.Error())
		return err
	}
	dbDest := path.Join(deploy.GiteaStatePath(opts.RemoteDir), databaseRelPath())
	if err := streamFileToRemote(ctx, opts.Host, filepath.Join(plainDir, filepath.FromSlash(dbComponent.Path)), dbDest); err != nil {
		err = fmt.Errorf("restore: place database: %w", err)
		job.Emit(StepPlaceState, events.StateFailed, err.Error())
		return err
	}

	if err := deploy.ChownState(ctx, opts.Host, opts.RemoteDir); err != nil {
		err = fmt.Errorf("restore: %w", err)
		job.Emit(StepPlaceState, events.StateFailed, err.Error())
		return err
	}

	// The image is the snapshot's own pinned Forgejo (RSTR-002) rather than
	// the target bundle's, because that is the one runDeploy is about to
	// start against this state — checking access as any other image would
	// be checking a deployment that is not going to happen.
	if err := deploy.VerifyForgeCanUsePlacedState(ctx, opts.Host, manifest.ForgejoVersion, opts.RemoteDir, repoDirs, []string{dbDest}); err != nil {
		err = fmt.Errorf("restore: %w", err)
		job.Emit(StepPlaceState, events.StateFailed, err.Error())
		return err
	}

	// The database now on this host is the one the snapshot captured, so the
	// version that wrote it is the snapshot's, whatever the host held before
	// — a scratch target reused between drills may well have held something
	// else. Stamping it here, next to the database it describes, is what
	// lets runDeploy's own deploy.Up see the version it is about to boot
	// (manifest.ForgejoVersion, RSTR-002) match the state in front of it and
	// proceed without an exemption: a restore never migrates (UPGR-003,
	// spec.md "Version pinning"), and this is where that becomes checkable
	// rather than merely true.
	if err := deploy.RecordStateVersion(ctx, opts.Host, opts.RemoteDir, manifest.ForgejoVersion); err != nil {
		err = fmt.Errorf("restore: %w", err)
		job.Emit(StepPlaceState, events.StateFailed, err.Error())
		return err
	}

	job.Emit(StepPlaceState, events.StateSucceeded, fmt.Sprintf("restored %d repository(ies) and the database", len(names)))
	return nil
}

// placeOneRepo extracts p's object archive into <gitRoot>/<name>.git, then
// extracts its ref archive on top. The order matters: the object archive
// was captured after BKUP-002's push hold released and can carry a newer
// ref state than the hold pinned, from a push that landed in the gap
// between release and archive capture (capture.go's captureGitObjects doc
// comment) — objects are content-addressed and immutable, so the extra
// objects such a push added are harmless, but its ref state is not the
// state the database component (captured during the same hold as the ref
// archive) is consistent with. Applying the ref archive second restores
// exactly that hold-time, database-consistent state.
func placeOneRepo(ctx context.Context, host Host, plainDir, gitRoot, name string, p gitPair) error {
	dir := repoDirPath(gitRoot, name)
	if err := extractRemoteTar(ctx, host, filepath.Join(plainDir, filepath.FromSlash(p.objectsPath)), dir); err != nil {
		return fmt.Errorf("restore: place git: %s: objects: %w", name, err)
	}
	if err := extractRemoteTar(ctx, host, filepath.Join(plainDir, filepath.FromSlash(p.refsPath)), dir); err != nil {
		return fmt.Errorf("restore: place git: %s: refs: %w", name, err)
	}
	return nil
}

// repoDirPath is the host directory one repository's git data is restored
// into — the layout Forgejo itself expects under forge.RepoRoot. Named
// once because placeState hands the same paths to the access check that
// placeOneRepo extracted into, and a check aimed a directory off the ones
// that were actually written would pass for the wrong reason.
func repoDirPath(gitRoot, name string) string {
	return path.Join(gitRoot, name+".git")
}

// gitComponentPairs groups manifest's git components by repository name
// into objects/refs pairs, refusing loudly — naming exactly what's missing
// — if either half is absent for a name the manifest does declare: a
// snapshot with one half of a repository's git data is a torn snapshot,
// and restore must not silently proceed with only the objects or only the
// refs (CLAUDE.md: "a torn or incomplete snapshot must refuse loudly and
// name the specific missing state").
func gitComponentPairs(manifest *backup.Manifest) (map[string]gitPair, []string, error) {
	pairs := make(map[string]gitPair)
	for _, c := range manifest.Components {
		if c.Kind != bundle.StateKindGit {
			continue
		}
		p := pairs[c.Name]
		switch {
		case strings.HasSuffix(c.Path, ".refs.tar"):
			p.refsPath = c.Path
		case strings.HasSuffix(c.Path, ".tar"):
			p.objectsPath = c.Path
		default:
			return nil, nil, fmt.Errorf("restore: git component %s: unrecognized snapshot path %q", c.Name, c.Path)
		}
		pairs[c.Name] = p
	}

	names := make([]string, 0, len(pairs))
	for name, p := range pairs {
		if p.objectsPath == "" {
			return nil, nil, fmt.Errorf("restore: git component %s: snapshot is torn — missing the object archive", name)
		}
		if p.refsPath == "" {
			return nil, nil, fmt.Errorf("restore: git component %s: snapshot is torn — missing the ref archive", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return pairs, names, nil
}

// databaseComponent returns manifest's one database component.
// backup.Verify's completeness check already refuses a manifest without
// exactly one before restore ever calls this; the count check here guards
// against a caller skipping that step rather than a real, reachable case.
func databaseComponent(manifest *backup.Manifest) (backup.Component, error) {
	var found []backup.Component
	for _, c := range manifest.Components {
		if c.Kind == bundle.StateKindDatabase {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		return backup.Component{}, fmt.Errorf("restore: manifest has %d database component(s), want exactly 1", len(found))
	}
	return found[0], nil
}

// databaseRelPath is the database file's path relative to forge.DataPath —
// "gitea.db" today — derived from forge.DatabasePath rather than
// hardcoded, so restore stays correct if that relationship ever changes.
func databaseRelPath() string {
	return strings.TrimPrefix(forge.DatabasePath, forge.DataPath+"/")
}

// extractRemoteTar streams the local tar archive at localPath into
// targetDir on host, extracting it there — the inverse of
// backup.SSHGitCapturer.Archive/Refs, which tar a remote directory's
// contents out over the same kind of SSH session.
func extractRemoteTar(ctx context.Context, host Host, localPath, targetDir string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", localPath, err)
	}
	defer f.Close()

	command := fmt.Sprintf("mkdir -p %s && tar -C %s -xf -", restoreShQuote(targetDir), restoreShQuote(targetDir))
	var stderr strings.Builder
	if err := host.RunStdin(ctx, command, f, nil, &stderr); err != nil {
		return fmt.Errorf("extract onto %s: %w%s", targetDir, err, restoreStderrSuffix(stderr.String()))
	}
	return nil
}

// streamFileToRemote streams the local file at localPath to remotePath on
// host, creating remotePath's parent directory as needed.
func streamFileToRemote(ctx context.Context, host Host, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", localPath, err)
	}
	defer f.Close()

	dir := path.Dir(remotePath)
	command := fmt.Sprintf("mkdir -p %s && cat > %s", restoreShQuote(dir), restoreShQuote(remotePath))
	var stderr strings.Builder
	if err := host.RunStdin(ctx, command, f, nil, &stderr); err != nil {
		return fmt.Errorf("write %s: %w%s", remotePath, err, restoreStderrSuffix(stderr.String()))
	}
	return nil
}

// restoreShQuote wraps s in single quotes for safe use as one argument in a
// /bin/sh command line, escaping any single quote it contains — the same
// helper every package that builds a remote shell command from a path
// defines for itself (deploy.stateShQuote, backup's gitShellQuote and
// shellQuote).
func restoreShQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func restoreStderrSuffix(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return fmt.Sprintf(" (stderr: %s)", stderr)
}
