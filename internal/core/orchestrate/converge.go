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
// directory swapped in with mv — the same staging-then-swap approach
// bundle.Bundle.Save uses locally — so a converge always leaves the host
// with exactly the files in b.Compose, no more and no less: a component
// removed from the manifest since the last converge stops being shipped,
// and --remove-orphans stops its container.
//
// The swap is not atomic, in the same way Save's is not: the install step
// removes the old directory and then renames the staging one into place, so
// a crash between the two leaves remoteDir/compose absent. Nothing reads it
// in that window — Converge runs docker compose only after the swap, and a
// host has one control plane — and the next converge re-ships the whole
// definition, so the repair is to run it again. Making the replacement
// genuinely atomic needs a different shape on both sides (a symlink flipped
// with rename), which Save defers as out of scope at CORE-001.
//
// docker compose up -d is itself idempotent and recreates any service
// whose definition changed, so Converge is safe to run against a host
// that's already converged, freshly provisioned, or has drifted.
//
// Host state is disposable (spec.md "Stateless vs. stateful"): Converge
// never reads back what is already running before deciding what to do. It
// always writes the full Compose definition and lets docker compose
// reconcile, so the bundle alone determines the outcome.
func Converge(ctx context.Context, t Transport, remoteDir string, b *bundle.Bundle) error {
	if strings.TrimSpace(remoteDir) == "" {
		return fmt.Errorf("orchestrate: converge: remote directory is required")
	}
	if b == nil || len(b.Compose) == 0 {
		return fmt.Errorf("orchestrate: converge: bundle has no rendered Compose files")
	}

	dir := path.Join(remoteDir, composeDir)
	staging := dir + ".tmp"

	if _, err := t.Output(ctx, fmt.Sprintf("rm -rf %s && mkdir -p %s", shQuote(staging), shQuote(staging))); err != nil {
		return fmt.Errorf("orchestrate: converge: stage %s: %w", staging, err)
	}

	// Pinned here, after the staging directory has created remoteDir and
	// before the swap installs remoteDir/compose. PinProjectName decides
	// what to write by whether shipped Compose files are already there, and
	// that question has to be asked about the previous converge's files,
	// not this one's.
	if err := PinProjectName(ctx, t, remoteDir, b); err != nil {
		return fmt.Errorf("orchestrate: converge: %w", err)
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

	if _, err := t.Output(ctx, fmt.Sprintf("rm -rf %s && mv %s %s", shQuote(dir), shQuote(staging), shQuote(dir))); err != nil {
		return fmt.Errorf("orchestrate: converge: install %s: %w", dir, err)
	}

	prefix, err := ComposeCommand(remoteDir, b)
	if err != nil {
		return fmt.Errorf("orchestrate: converge: %w", err)
	}
	up := prefix + " docker compose up -d --remove-orphans"
	if _, err := t.Output(ctx, up); err != nil {
		return fmt.Errorf("orchestrate: converge: docker compose up: %w", err)
	}
	return nil
}
