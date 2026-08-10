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

// GitStatePath, GiteaStatePath, and BlobsStatePath return the host-side
// directories forge state lives under (tech-spec.md "Host state layout
// (UP-004)") — the one layout decision every caller that needs to know
// where a bundle's state lives on a host must agree on, rather than each
// spelling out "<RemoteDir>/state/git" independently. configureState
// builds and mounts them; backup.BuildOptions points state.SSHGitExporter
// at GitStatePath so it captures from the same place configureState
// mounted; restore.Restore places a snapshot's git data and database back
// at GitStatePath and GiteaStatePath before deploy.Up ever starts the
// forgejo container that reads them.
func GitStatePath(remoteDir string) string {
	return path.Join(remoteDir, stateDir, gitStateDir)
}

func GiteaStatePath(remoteDir string) string {
	return path.Join(remoteDir, stateDir, giteaStateDir)
}

func BlobsStatePath(remoteDir string) string {
	return path.Join(remoteDir, stateDir, blobsStateDir)
}

// ChownState recursively gives GitStatePath and GiteaStatePath to the
// uid:gid the forgejo container runs as. configureState's own chown (below)
// only touches each directory's top level — enough for `up`, which always
// starts from an empty bind mount that only Forgejo itself, running as that
// uid, ever writes into afterward. restore.Restore populates these
// directories itself, over an SSH session that generally isn't
// forgeUID:forgeGID, before Forgejo ever starts — so it calls this once
// every file it wrote is in place, to bring all of it, not just the top
// directory, under the ownership Forgejo needs to read and write it.
//
// Like every other chown in this package it is best-effort (access.go): on
// a host whose container runtime maps ownership across the mount boundary
// it cannot succeed and does not need to, and refusing to restore over that
// would be refusing over a mechanism rather than an outcome. What is
// insisted on instead is the outcome, checked by whoever placed the
// content over the paths they placed: restore.placeState calls
// VerifyForgeCanUsePlacedState on the repository directories and the
// database file it just wrote, and fails the restore if the forge cannot
// use them.
//
// The deploy.Up that runs next is not that check and does not cover this.
// Its own probe (configureState) touches the top of each state directory
// and nothing beneath, which is right for `up` — it creates nothing else —
// but on a target whose state directories already exist and are already
// forge-owned it passes while everything this chown failed to reach stays
// unusable. access.go "Each caller verifies what it placed" has the whole
// division. The error return is kept for a failure that is about the host
// rather than about ownership.
func ChownState(ctx context.Context, host Host, remoteDir string) error {
	chownBestEffort(ctx, host, true, GitStatePath(remoteDir), GiteaStatePath(remoteDir))
	return nil
}

// forgeUID and forgeGID are the uid:gid the official Forgejo image's git
// user runs as — USER_UID and USER_GID both default to 1000 in the
// upstream image, and Farrier's rendered app.ini (forge.RenderAppINI) never
// overrides them. A bind-mounted host directory must be usable by this
// uid:gid before the forgejo container starts: Docker creates a missing
// bind-mount source root-owned, and Forgejo drops root inside its
// entrypoint before it ever touches its data directories, so a root-owned
// mount leaves it unable to write. Making the directories usable is a
// chown on some hosts and is arranged by the container runtime on others;
// access.go has the whole story, and holds the check that decides whether
// this deployment has it either way.
const (
	forgeUID = 1000
	forgeGID = 1000
)

// configureState gives forge state a durable home on the host (UP-004): it
// creates <RemoteDir>/state/git and <RemoteDir>/state/gitea, makes them
// usable by the uid:gid the forgejo container runs as, and returns compose
// with both bind-mounted into the forgejo service at the container-side
// paths forge.RenderAppINI already assumes (forge.RepoRoot,
// forge.DataPath). It reports whether it was able to set ownership itself,
// so the caller can say so — the deployment is fine either way, and which
// way it went is the one interesting thing about this step.
//
// "Usable" is verified, not assumed. The chown that arranges it on a Linux
// host cannot be applied on a macOS one, where the container runtime maps
// ownership across the mount boundary instead — so the chown is attempted
// and not insisted on, and what this step actually refuses to continue past
// is a forge that cannot read and write these directories
// (verifyForgeCanUseState). access.go carries the reasoning.
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
// It also creates the empty file the rendered app.ini bind-mounts onto
// (appINIMountpointScript). That mount's target sits *inside* this one, and
// a container runtime that has to create a missing target under a
// host-side mount cannot always do it; creating it here first is what makes
// `up` reach a running forgejo container on every host rather than most of
// them.
//
// Creating the directories, creating that file, chowning them, and probing
// them are all idempotent — mkdir -p and chown leave existing contents
// untouched, the mountpoint is only created when absent, and the probe
// removes what it wrote — so a re-run against a host that already has this
// layout changes nothing (UP-003).
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
func configureState(ctx context.Context, host Host, b *bundle.Bundle, remoteDir string, compose map[string][]byte) (map[string][]byte, bool, error) {
	gitPath := GitStatePath(remoteDir)
	giteaPath := GiteaStatePath(remoteDir)

	if err := ensureDirs(ctx, host, gitPath, giteaPath); err != nil {
		return nil, false, fmt.Errorf("create state directories: %w", err)
	}
	if _, err := host.Output(ctx, appINIMountpointScript(giteaPath)); err != nil {
		return nil, false, fmt.Errorf("create the file app.ini mounts onto: %w", err)
	}
	owned := chownBestEffort(ctx, host, false, gitPath, giteaPath)
	if err := verifyForgeCanUseState(ctx, host, b.Manifest.Images[forge.Service], remoteDir); err != nil {
		return nil, owned, err
	}

	if b.Manifest.Drivers.Blob.Driver == "local" {
		blobsPath := BlobsStatePath(remoteDir)
		if _, err := host.Output(ctx, fmt.Sprintf("mkdir -p %s", stateShQuote(blobsPath))); err != nil {
			return nil, owned, fmt.Errorf("create blobs state directory: %w", err)
		}
	}

	compose, err := orchestrate.WithBindMount(compose, forge.Service, gitPath, forge.RepoRoot)
	if err != nil {
		return nil, owned, fmt.Errorf("mount git state: %w", err)
	}
	compose, err = orchestrate.WithBindMount(compose, forge.Service, giteaPath, forge.DataPath)
	if err != nil {
		return nil, owned, fmt.Errorf("mount gitea state: %w", err)
	}
	return compose, owned, nil
}

// appINIRelPath is forge.AppINIPath's location relative to forge.DataPath
// — "conf/app.ini" today — derived rather than hardcoded a second time,
// the same reason sshHostKeyRelPath derives from forge.SSHHostKeyPath.
func appINIRelPath() string {
	return strings.TrimPrefix(forge.AppINIPath, forge.DataPath+"/")
}

// appINIMountpointScript builds the /bin/sh program that creates the file
// the rendered app.ini bind-mounts onto, inside the gitea state directory
// at giteaPath.
//
// Two of the forgejo service's bind mounts overlap by design: this
// directory mounts at forge.DataPath, and the app.ini Up ships under
// <RemoteDir>/forge mounts at forge.AppINIPath, which is inside it. So the
// second mount's target — the host file at <giteaPath>/conf/app.ini —
// falls under the first mount, and a container runtime that finds it
// missing has to create it there. On Docker Desktop it will not: that path
// reaches the host through virtiofs, and runc refuses to create a
// mountpoint outside the container rootfs, failing the whole converge with
// every image pulled and every container created. On Linux, where the
// runtime creates it without ceremony, this has simply already been done.
//
// Which is the point: this is one command for every host, not a branch on
// which host it is. Creating a mountpoint the runtime would have created
// anyway costs nothing on the hosts that can, and detecting the target's
// operating system — or whether it is local at all — is exactly the
// locality-dependent behavior ORCH-003 exists to rule out (access.go says
// the same about ownership).
//
// The file is created only when it is absent, and never truncated. A live
// instance can have a real app.ini here — restored state, or a converge
// that ran before this mount existed — and emptying it during a routine
// `up` would be a worse failure than the one this fixes. `touch` rather
// than a redirection for the same reason, and guarded by the existence test
// so a re-run does not even move an existing file's mtime.
func appINIMountpointScript(giteaPath string) string {
	mountpoint := path.Join(giteaPath, appINIRelPath())
	return fmt.Sprintf(
		"mkdir -p %s && { [ -e %s ] || touch %s; }",
		stateShQuote(path.Dir(mountpoint)), stateShQuote(mountpoint), stateShQuote(mountpoint),
	)
}

// ensureDirs creates every directory in dirs on host, in a single command
// so a partial run never leaves some created and others not.
func ensureDirs(ctx context.Context, host Host, dirs ...string) error {
	quoted := make([]string, len(dirs))
	for i, d := range dirs {
		quoted[i] = stateShQuote(d)
	}
	command := fmt.Sprintf("mkdir -p %s", strings.Join(quoted, " "))
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
