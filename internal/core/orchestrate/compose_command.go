package orchestrate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
)

// LegacyProjectName is the Docker Compose project name every Farrier
// deployment used before the project name became a per-deployment identity,
// and the name a deployment placed under it keeps for the rest of its life
// (see ProjectFile).
//
// It is also the fallback ComposeCommand resolves to on a host carrying no
// record at all, which is exactly the shape of a deployment made by an
// older binary. Nothing that already exists is orphaned by the change.
const LegacyProjectName = "farrier"

// ProjectFile is the file directly under a deployment's remote directory
// recording which Docker Compose project that deployment's containers
// belong to (tech-spec.md "Host state layout").
//
// It exists because Compose resolves a project's containers by a label
// carrying the project name, and by nothing else — not by directory, not by
// the file list it was given. One name shared by every deployment therefore
// made every deployment on a host one project, and a teardown of any of
// them a teardown of all of them. A drill on the machine hosting the live
// instance removed the live instance's containers, because from the drill's
// point of view they were its own.
//
// The record lives on the host, beside the deployment it names, rather than
// in the bundle: two deployments of one bundle — the live instance and a
// drill of its own snapshot — are two deployments and must be two projects,
// so the answer cannot come from the bundle alone. It sits above state/
// rather than inside it because restore places state/ from a snapshot, and
// which project a host's containers run under is a property of the host,
// never of the snapshot.
const ProjectFile = "compose-project"

// ProjectPath returns the host-side path of that record. It is the single
// spelling of this layout decision, the same way deploy.StateVersionPath is
// for the record beside it.
func ProjectPath(remoteDir string) string {
	return path.Join(remoteDir, ProjectFile)
}

// maxProjectSlug bounds the readable half of a derived project name. The
// slug is there so `docker ps` says which instance a container belongs to;
// a domain long enough to push past this has already stopped being
// readable, and the digest that follows is what actually distinguishes
// deployments.
const maxProjectSlug = 40

// ProjectNameFor derives the Compose project name for a *new* deployment of
// b at remoteDir: a readable slug of the bundle's domain, plus a short
// digest of that domain and the remote directory together.
//
// Both inputs are load-bearing. The remote directory is what separates two
// deployments of the same bundle on one host — a drill restores the live
// instance's own snapshot from the live instance's own bundle, so the
// bundle alone cannot tell them apart, and the directory is the only thing
// that can. The domain is what separates two different bundles, so that
// nothing depends on operators having chosen different directories.
//
// Neither input varies per run. Every command that addresses a deployment —
// `up`, `backup`, `status`, `upgrade`, the admin bootstrap `up` runs after
// converging — is given the same remote directory and the same bundle, so a
// derived name is the same name each time.
//
// It is derived once all the same. The result is written to ProjectPath at
// the deployment's first converge and read back from there afterwards (see
// PinProjectName), so an operator who changes the manifest under a running
// instance cannot rename the project out from under its own containers.
// `attach` (UP-007) is precisely that case: it fills a domain into a
// nameless bundle in place, on a host that is already deployed.
func ProjectNameFor(remoteDir string, b *bundle.Bundle) string {
	domain := ""
	if b != nil {
		domain = strings.TrimSpace(b.Manifest.Domain)
	}

	sum := sha256.Sum256([]byte(domain + "\x00" + path.Clean(remoteDir)))
	digest := fmt.Sprintf("%x", sum[:4])

	slug := projectSlug(domain)
	if slug == "" {
		return LegacyProjectName + "-" + digest
	}
	return LegacyProjectName + "-" + slug + "-" + digest
}

// projectSlug reduces s to the character set a Compose project name allows
// — lowercase letters, digits, dashes — collapsing every run of anything
// else into a single dash and trimming the dashes off both ends. It returns
// the empty string when s carries nothing usable, which a nameless bundle's
// absent domain and a domain of pure punctuation both do.
//
// Sanitizing rather than trusting the manifest is the point: the domain is
// operator input, Compose rejects a project name outside that set, and a
// rejected project name is a deployment that cannot be reached at all.
func projectSlug(s string) string {
	var out strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
			dash = false
		default:
			if !dash && out.Len() > 0 {
				out.WriteByte('-')
				dash = true
			}
		}
		if out.Len() >= maxProjectSlug {
			break
		}
	}
	return strings.Trim(out.String(), "-")
}

// PinProjectName records which Compose project the deployment at remoteDir
// belongs to, once, and never changes it afterwards. Converge calls it
// before it ships anything, so every command that reaches the host after
// the first converge — including the `docker compose up` that converge
// itself runs — resolves the same name.
//
// The name it writes depends on what is already there, because a host
// deployed by an older binary is running containers under
// LegacyProjectName and must keep resolving them:
//
//   - A record already present is left alone. The name a deployment was
//     created under is the name its containers carry, and re-deriving it
//     later would orphan them.
//   - A remote directory that already holds shipped Compose files, with no
//     record beside them, is a deployment an older binary made. It is
//     pinned to LegacyProjectName — its containers answer to that name, and
//     nothing about them changes.
//   - Anything else is a directory no converge has run in yet, so it takes
//     the derived per-deployment name.
//
// The decision is made on the host, in one command, because only the host
// can see which of the three cases applies.
func PinProjectName(ctx context.Context, t Transport, remoteDir string, b *bundle.Bundle) error {
	record := shQuote(ProjectPath(remoteDir))
	shipped := shQuote(path.Join(remoteDir, composeDir))
	derived := shQuote(ProjectNameFor(remoteDir, b))
	legacy := shQuote(LegacyProjectName)

	pin := fmt.Sprintf(
		"if [ ! -s %s ]; then if [ -d %s ]; then printf '%%s\\n' %s > %s; else printf '%%s\\n' %s > %s; fi; fi",
		record, shipped, legacy, record, derived, record,
	)
	if _, err := t.Output(ctx, pin); err != nil {
		return fmt.Errorf("orchestrate: pin compose project at %s: %w", ProjectPath(remoteDir), err)
	}
	return nil
}

// ComposeCommand returns the shell prefix that reaches the exact project
// and files Converge started at remoteDir: a `cd` into remoteDir, plus
// COMPOSE_PROJECT_NAME and COMPOSE_FILE set from b's rendered Compose
// files in the same sorted order Converge ships them in. A caller appends
// its own docker compose subcommand — "docker compose up -d
// --remove-orphans", "docker compose exec -T forgejo ..." — to run it
// against that same deployment, without re-deriving Converge's file list,
// project name, or quoting.
//
// The project name is resolved on the host, from the record Converge pinned
// at ProjectPath, rather than computed here. The record is the only place
// that knows which project a host's containers actually run under: a
// deployment made by an older binary runs under LegacyProjectName, which is
// why that is what an absent or empty record falls back to. Resolving it in
// the same shell as the command it prefixes also means there is no window
// in which a command could address a project the host has since renamed.
func ComposeCommand(remoteDir string, b *bundle.Bundle) (string, error) {
	if strings.TrimSpace(remoteDir) == "" {
		return "", fmt.Errorf("orchestrate: compose command: remote directory is required")
	}
	if b == nil || len(b.Compose) == 0 {
		return "", fmt.Errorf("orchestrate: compose command: bundle has no rendered Compose files")
	}

	names := make([]string, 0, len(b.Compose))
	for name := range b.Compose {
		names = append(names, name)
	}
	sort.Strings(names)

	paths := make([]string, len(names))
	for i, name := range names {
		paths[i] = path.Join(composeDir, name)
	}

	record := shQuote(ProjectPath(remoteDir))
	// `-s` rather than `-f`: a record that exists but is empty is a torn
	// write, and an empty COMPOSE_PROJECT_NAME is not a project Compose can
	// resolve. Falling back is the same answer an absent record gets.
	project := fmt.Sprintf("\"$(if [ -s %s ]; then cat %s; else printf '%%s' %s; fi)\"",
		record, record, shQuote(LegacyProjectName))

	return fmt.Sprintf("cd %s && COMPOSE_PROJECT_NAME=%s COMPOSE_FILE=%s",
		shQuote(remoteDir), project, shQuote(strings.Join(paths, ":"))), nil
}
