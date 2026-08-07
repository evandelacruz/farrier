package orchestrate

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
)

// ProjectName is the Docker Compose project name every Farrier deployment
// uses. Anything that reaches an already-converged host — Converge itself,
// or a later command against the same deployment (FORGE-002's admin
// bootstrap) — addresses containers under this one project name, so they
// agree on which containers they mean.
const ProjectName = "farrier"

// ComposeCommand returns the shell prefix that reaches the exact project
// and files Converge started at remoteDir: a `cd` into remoteDir, plus
// COMPOSE_PROJECT_NAME and COMPOSE_FILE set from b's rendered Compose
// files in the same sorted order Converge ships them in. A caller appends
// its own docker compose subcommand — "docker compose up -d
// --remove-orphans", "docker compose exec -T forgejo ..." — to run it
// against that same deployment, without re-deriving Converge's file list,
// project name, or quoting.
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

	return fmt.Sprintf("cd %s && COMPOSE_PROJECT_NAME=%s COMPOSE_FILE=%s",
		shQuote(remoteDir), ProjectName, shQuote(strings.Join(paths, ":"))), nil
}
