package deploy

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// stateDir is the directory under RemoteDir forge state lives in
// (tech-spec.md "Host state layout (UP-004)") — the one part of RemoteDir
// that isn't disposable the way the rest of a deployment is (spec.md
// "Stateless vs. stateful").
const stateDir = "state"

// State subdirectory names under stateDir.
const (
	gitStateDir   = "git"
	giteaStateDir = "gitea"
	blobsStateDir = "blobs"
)

// forgeUID and forgeGID are the uid:gid the official Forgejo image's git
// user runs as — USER_UID and USER_GID both default to 1000 in the
// upstream image, and Farrier's rendered app.ini (forge.RenderAppINI) never
// overrides them. A bind-mounted host directory must already be owned by
// this uid:gid before the forgejo container starts: Docker creates a
// missing bind-mount source root-owned, and Forgejo drops root inside its
// entrypoint before it ever touches its data directories, so a root-owned
// mount leaves it unable to write.
const (
	forgeUID = 1000
	forgeGID = 1000
)

// configureState gives forge state a durable home on the host (UP-004): it
// creates <RemoteDir>/state/git and <RemoteDir>/state/gitea, owned by the
// uid:gid the forgejo container runs as, and returns compose with both
// bind-mounted into the forgejo service at the container-side paths
// forge.RenderAppINI already assumes (forge.RepoRoot, forge.DataPath).
//
// Without this, git repositories and the SQLite database live in the
// forgejo container's writable layer, and Converge's
// `docker compose up -d --remove-orphans` — which recreates any service
// whose resolved config changed — destroys them on the very next `up`
// (spec.md "Stateless vs. stateful"). forge.DataPath also covers LFS
// objects, attachments, avatars, and CI artifacts: RenderAppINI stores all
// of them under it, unconditionally, so this one mount keeps every kind of
// state Forgejo itself writes durable, not just the database.
//
// Creating the directories and chowning them are both idempotent — mkdir
// -p and chown leave existing contents untouched — so a re-run against a
// host that already has this layout changes nothing (UP-003).
//
// When the bundle's blob driver is "local", configureState also creates
// <RemoteDir>/state/blobs, the host directory tech-spec.md "Host state
// layout" reserves for it. It is deliberately not bind-mounted into the
// forgejo service, and not chowned to forgeUID:forgeGID: nothing in the
// tree today configures Forgejo's own storage to write anywhere other than
// forge.DataPath, so there is no container-side path yet for a local blob
// adapter's content to land at, and no reason for the forgejo container's
// user to own it. This creates the directory tech-spec.md names so it
// exists ahead of that wiring, without this change guessing at what it
// will be.
func configureState(ctx context.Context, host Host, b *bundle.Bundle, remoteDir string, compose map[string][]byte) (map[string][]byte, error) {
	gitPath := path.Join(remoteDir, stateDir, gitStateDir)
	giteaPath := path.Join(remoteDir, stateDir, giteaStateDir)

	if err := ensureOwnedDirs(ctx, host, gitPath, giteaPath); err != nil {
		return nil, fmt.Errorf("create state directories: %w", err)
	}
	if b.Manifest.Drivers.Blob.Driver == "local" {
		blobsPath := path.Join(remoteDir, stateDir, blobsStateDir)
		if _, err := host.Output(ctx, fmt.Sprintf("mkdir -p %s", stateShQuote(blobsPath))); err != nil {
			return nil, fmt.Errorf("create blobs state directory: %w", err)
		}
	}

	compose, err := orchestrate.WithBindMount(compose, forge.Service, gitPath, forge.RepoRoot)
	if err != nil {
		return nil, fmt.Errorf("mount git state: %w", err)
	}
	compose, err = orchestrate.WithBindMount(compose, forge.Service, giteaPath, forge.DataPath)
	if err != nil {
		return nil, fmt.Errorf("mount gitea state: %w", err)
	}
	return compose, nil
}

// ensureOwnedDirs creates every directory in dirs on host and chowns each
// to forgeUID:forgeGID, in a single command so a partial run never leaves
// some created and owned correctly and others not.
func ensureOwnedDirs(ctx context.Context, host Host, dirs ...string) error {
	quoted := make([]string, len(dirs))
	for i, d := range dirs {
		quoted[i] = stateShQuote(d)
	}
	joined := strings.Join(quoted, " ")
	command := fmt.Sprintf("mkdir -p %s && chown %d:%d %s", joined, forgeUID, forgeGID, joined)
	_, err := host.Output(ctx, command)
	return err
}

// stateShQuote wraps s in single quotes for safe use as one argument in a
// /bin/sh command line, escaping any single quotes it contains. remoteDir
// comes from the caller (Options.RemoteDir), so a path with a space or a
// shell metacharacter must not split into several arguments or run
// something this package never wrote.
func stateShQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
