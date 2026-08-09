package deploy

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// composeStopTimeout bounds, in seconds, how long `docker compose down`
// waits for each container to stop before killing it. A teardown that hangs
// on a container ignoring SIGTERM is a teardown that does not happen, and
// nothing under RemoteDir is worth a graceful shutdown: the deployment being
// torn down is disposable by construction — its state either lives in a
// snapshot already or was never meant to outlive the command that placed it.
const composeStopTimeout = 30

// Down removes the deployment Up placed on host at remoteDir: it stops and
// removes the Compose project's containers, networks, and named volumes,
// then removes remoteDir itself, leaving the host with none of the
// deployment's containers and none of its files.
//
// It is the mirror of Up and the inverse of UP-004. Up gives forge state a
// durable home on the host under remoteDir/state, deliberately outside any
// container, so recreating a container never destroys it; `docker compose
// down` therefore cannot remove it, and neither can anything else that only
// addresses containers. Removing remoteDir is what actually makes the host
// clean, and it is also what removes the deploy-time key material Up wrote
// there — the rendered app.ini, the runner registration secret, the SSH
// host key (KEY-003) — none of which should outlive the deployment they
// were written for.
//
// Down is destructive and irreversible for exactly that reason: everything
// under remoteDir is gone when it returns. `up` never calls it. Its caller
// is drill (DRIL-003), whose deployment is a rehearsal on a scratch target
// that is meant to be disposed of; anything that must survive a Down is a
// snapshot at a backup destination, not a directory on a host.
//
// Down is idempotent and safe against a host Up only partially deployed to,
// which is the case a failed drill leaves behind. If remoteDir holds no
// shipped Compose files, no compose project was ever started from it and
// the compose step is skipped rather than failed — the removal below still
// clears whatever state a half-finished deployment did place. Removing a
// directory that isn't there is likewise not an error.
//
// Down reports failure rather than repairing it. It returns an error naming
// the step that failed, and its caller is expected to surface that: a host
// that still holds a deployment after a teardown is something an operator
// needs to know about, not something to swallow.
func Down(ctx context.Context, host Host, b *bundle.Bundle, remoteDir string) error {
	if host == nil {
		return fmt.Errorf("deploy: down: host is required")
	}
	if strings.TrimSpace(remoteDir) == "" {
		return fmt.Errorf("deploy: down: remote directory is required")
	}

	prefix, err := orchestrate.ComposeCommand(remoteDir, b)
	if err != nil {
		return fmt.Errorf("deploy: down: %w", err)
	}

	// Guarded on the shipped Compose directory rather than run
	// unconditionally: `docker compose down` resolves the files named by
	// COMPOSE_FILE before it does anything, so running it against a host
	// that never got as far as Converge would fail on the missing files and
	// report a teardown failure where there was nothing to tear down.
	//
	// Addressing the project through ComposeCommand — the same project name
	// and the same file list Converge started it with — rather than by
	// project name alone is what makes `--remove-orphans` mean what it says
	// here: every container of that project, including any left by a
	// definition since removed from the manifest.
	shipped := path.Join(remoteDir, bundle.ComposeDir)
	down := fmt.Sprintf(
		"if [ -d %s ]; then %s docker compose down --volumes --remove-orphans --timeout %d; fi",
		stateShQuote(shipped), prefix, composeStopTimeout,
	)
	if _, err := host.Output(ctx, down); err != nil {
		return fmt.Errorf("deploy: down: docker compose down: %w", err)
	}

	if _, err := host.Output(ctx, fmt.Sprintf("rm -rf %s", stateShQuote(remoteDir))); err != nil {
		return fmt.Errorf("deploy: down: remove %s: %w", remoteDir, err)
	}
	return nil
}
