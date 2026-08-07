package orchestrate

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
)

// composeDir is the directory under remoteDir Converge ships rendered
// Compose files into, mirroring bundle.ComposeDir.
const composeDir = "compose"

// Converge ships b's rendered Compose files to remoteDir on the host
// reached through t and runs `docker compose up -d --remove-orphans`
// (ORCH-002). remoteDir/compose is replaced wholesale, via a staging
// directory swapped in with mv — the same atomic-swap approach
// bundle.Bundle.Save uses locally — so a converge always leaves the host
// with exactly the files in b.Compose, no more and no less: a component
// removed from the manifest since the last converge stops being shipped,
// and --remove-orphans stops its container.
//
// docker compose up -d is itself idempotent and recreates any service
// whose definition changed, so Converge is safe to run against a host
// that's already converged, freshly provisioned, or has drifted.
func Converge(ctx context.Context, t Transport, remoteDir string, b *bundle.Bundle) error {
	if strings.TrimSpace(remoteDir) == "" {
		return fmt.Errorf("orchestrate: converge: remote directory is required")
	}
	if b == nil || len(b.Compose) == 0 {
		return fmt.Errorf("orchestrate: converge: bundle has no rendered Compose files")
	}

	dir := path.Join(remoteDir, composeDir)
	staging := dir + ".tmp"

	if _, err := t.Run(ctx, fmt.Sprintf("rm -rf %s && mkdir -p %s", shQuote(staging), shQuote(staging))); err != nil {
		return fmt.Errorf("orchestrate: converge: stage %s: %w", staging, err)
	}

	names := make([]string, 0, len(b.Compose))
	for name := range b.Compose {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if err := t.WriteFile(ctx, path.Join(staging, name), b.Compose[name], 0o644); err != nil {
			return fmt.Errorf("orchestrate: converge: ship %s: %w", name, err)
		}
	}

	if _, err := t.Run(ctx, fmt.Sprintf("rm -rf %s && mv %s %s", shQuote(dir), shQuote(staging), shQuote(dir))); err != nil {
		return fmt.Errorf("orchestrate: converge: install %s: %w", dir, err)
	}

	files := make([]string, len(names))
	for i, name := range names {
		files[i] = "-f " + shQuote(path.Join(composeDir, name))
	}
	up := fmt.Sprintf("cd %s && docker compose --project-name farrier %s up -d --remove-orphans",
		shQuote(remoteDir), strings.Join(files, " "))
	if _, err := t.Run(ctx, up); err != nil {
		return fmt.Errorf("orchestrate: converge: docker compose up: %w", err)
	}
	return nil
}
