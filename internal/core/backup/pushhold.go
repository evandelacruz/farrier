package backup

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/caddy"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// PushHold rejects git pushes for the short, database-only window Run holds
// them across (BKUP-002, docs/spec.md "Backups"): the SQLite online backup
// plus recording every repository's ref state. It never covers the object
// tar — Run only starts that after Release returns — since git objects are
// immutable and append-only, so a push landing after release can only add
// objects, never disturb a ref already recorded during the hold.
//
// Engage must return only once pushes are being rejected; Release must
// return only once they flow again. Run calls Release exactly once per
// Engage on every exit path — success, error, panic, or a canceled context
// — so a capture that dies mid-hold never leaves pushes rejected until an
// operator notices.
type PushHold interface {
	Engage(ctx context.Context) error
	Release(ctx context.Context) error
}

// NoopPushHold holds nothing: pushes are never rejected. It's for
// topologies with no proxy in front of git traffic to reject at — a local
// capture, or a drill against a scratch checkout (DRIL-001) — where there's
// no live push traffic to protect against in the first place.
type NoopPushHold struct{}

// Engage does nothing.
func (NoopPushHold) Engage(ctx context.Context) error { return nil }

// Release does nothing.
func (NoopPushHold) Release(ctx context.Context) error { return nil }

// DefaultPushHoldMessage is the body Caddy returns to a rejected push —
// BKUP-002's requirement that the client gets a clean, immediate failure,
// not a queued or buffered request.
const DefaultPushHoldMessage = "farrier: backup in progress, git push rejected — please retry in a few seconds"

// holdCaddyfilePath is where CaddyPushHold stages the rejecting Caddyfile
// inside the Caddy container, distinct from caddy.ConfigPath — the one
// deploy.Up mounted and never modifies — so Release always has exactly
// that untouched file to reload back to, with nothing to save or restore.
const holdCaddyfilePath = "/etc/caddy/Caddyfile.push-hold"

// CaddyPushHold rejects git pushes by reloading the bundle's already-running
// Caddy with a temporary configuration that returns 503 for git's
// smart-HTTP push endpoints (caddy.RenderPushHoldCaddyfile), and releases
// by reloading Caddy back to the original Caddyfile at caddy.ConfigPath.
type CaddyPushHold struct {
	Runner state.Runner

	// Container is the Caddy container's name (e.g. "farrier-caddy").
	Container string

	// Domain and Upstream mirror caddy.RenderCaddyfile's own inputs — the
	// bundle domain Caddy terminates TLS for, and the forge address it
	// proxies to — so the hold configuration's routing matches production
	// exactly, aside from the two rejecting routes.
	Domain, Upstream string

	// Message is the body Caddy returns to a rejected push. Empty uses
	// DefaultPushHoldMessage.
	Message string
}

// Engage renders the push-hold Caddyfile and reloads Caddy against it.
func (c CaddyPushHold) Engage(ctx context.Context) error {
	if err := c.validate(); err != nil {
		return err
	}
	message := c.Message
	if message == "" {
		message = DefaultPushHoldMessage
	}
	config, err := caddy.RenderPushHoldCaddyfile(c.Domain, c.Upstream, message)
	if err != nil {
		return fmt.Errorf("backup: caddy push hold: render hold config: %w", err)
	}
	return c.reload(ctx, holdCaddyfilePath, config)
}

// Release reloads Caddy back to the original, untouched Caddyfile.
func (c CaddyPushHold) Release(ctx context.Context) error {
	if err := c.validate(); err != nil {
		return err
	}
	return c.reload(ctx, caddy.ConfigPath, nil)
}

func (c CaddyPushHold) validate() error {
	if c.Runner == nil {
		return errors.New("backup: caddy push hold: runner is required")
	}
	if strings.TrimSpace(c.Container) == "" {
		return errors.New("backup: caddy push hold: container is required")
	}
	return nil
}

// reload writes config into the container at path — skipped when config is
// nil, to reload a file already there, such as the original Caddyfile —
// then reloads Caddy against it. Content travels base64-encoded through the
// command string since Runner has no stdin, the same constraint
// SSHDatabaseExporter and SSHGitCapturer design around.
func (c CaddyPushHold) reload(ctx context.Context, path string, config []byte) error {
	var script strings.Builder
	if config != nil {
		encoded := base64.StdEncoding.EncodeToString(config)
		fmt.Fprintf(&script, "printf '%%s' %s | base64 -d > %s && ", gitShellQuote(encoded), gitShellQuote(path))
	}
	fmt.Fprintf(&script, "caddy reload --config %s --adapter caddyfile", gitShellQuote(path))

	command := fmt.Sprintf("docker exec %s sh -c %s", gitShellQuote(c.Container), gitShellQuote(script.String()))
	var stderr strings.Builder
	if err := c.Runner.Run(ctx, command, io.Discard, &stderr); err != nil {
		return fmt.Errorf("backup: caddy push hold: reload %s: %w%s", c.Container, err, gitCommandStderrSuffix(stderr.String()))
	}
	return nil
}
