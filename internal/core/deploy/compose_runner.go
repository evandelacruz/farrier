package deploy

import (
	"context"
	"fmt"
	"io"

	"github.com/evandelacruz/farrier/internal/core/bundle"
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

// Run prefixes command with the deployment's compose project reference and
// runs it on host.
func (r *composeRunner) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	prefix, err := orchestrate.ComposeCommand(r.remoteDir, r.bundle)
	if err != nil {
		return err
	}
	return r.host.Run(ctx, fmt.Sprintf("%s %s", prefix, command), stdout, stderr)
}
