package deploy

import (
	"context"
	"fmt"
	"io"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// composeRunner adapts a Host into a forge.Runner whose commands land in
// the exact Compose project Converge just started: forge.Bootstrap builds
// a bare "docker compose exec -T forgejo ..." command with no knowledge of
// remoteDir or the project name, so composeRunner prefixes every command
// with orchestrate.ComposeCommand's cd and COMPOSE_PROJECT_NAME/
// COMPOSE_FILE — the same project reference Converge itself uses — rather
// than have forge duplicate orchestrate's remote-directory and file-list
// bookkeeping.
type composeRunner struct {
	host      Host
	remoteDir string
	bundle    *bundle.Bundle
}

// ComposeRunner adapts an already-deployed host into a forge.Runner: every
// command it runs lands in the Compose project Up converged at remoteDir,
// the same way Up's own post-converge steps (admin bootstrap, runner
// registration) reach it.
//
// It is exported for callers that run something against the instance after
// Up has returned rather than as part of it — internal/core/drill's smoke CI
// job (DRIL-001), which needs a booted instance before it has anything to
// dispatch. b is only read for the names of its Compose files, so passing
// the bundle Up was given rather than the deploy-time-layered copy it
// shipped is correct: layering changes those files' contents, never their
// names.
func ComposeRunner(host Host, remoteDir string, b *bundle.Bundle) forge.Runner {
	return &composeRunner{host: host, remoteDir: remoteDir, bundle: b}
}

// Run prefixes command with the deployment's compose project reference and
// runs it on host.
func (r *composeRunner) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	prefix, err := orchestrate.ComposeCommand(r.remoteDir, r.bundle)
	if err != nil {
		return err
	}
	return r.host.Run(ctx, fmt.Sprintf("%s %s", prefix, command), stdout, stderr)
}
