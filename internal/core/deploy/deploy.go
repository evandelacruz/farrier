// Package deploy implements UP-001: deploying the full stateless layer
// (spec.md "Stateless vs. stateful" — the forge app, CI orchestration, and
// runners) to a target host given only an ssh://user@host address and a
// bundle.
//
// It is the sequencing layer over packages that already do the real work:
// orchestrate (SSH transport, Compose rendering and convergence, ORCH-001
// and ORCH-002) and forge (app.ini rendering and admin bootstrap, FORGE-001
// and FORGE-002). Up is the first thing that calls them together, in the
// order a real deployment needs.
package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// Step identifiers Up emits through the job's event stream (CORE-002), in
// the order it runs them. forge.StepAdminBootstrap follows StepWaitForge.
const (
	StepCheckHost      = "check-host"
	StepConfigureForge = "configure-forge"
	StepConverge       = "converge"
	StepWaitForge      = "wait-forge"
)

// hostConfigDir is the directory under RemoteDir Up writes deploy-time
// forge configuration to. It is deploy-time content, not bundle content
// (KEY-003; see forge.RenderAppINI's doc comment), so it lives alongside
// the shipped Compose files rather than inside the bundle directory itself.
const hostConfigDir = "forge"

// appINIFilename is the file Up ships app.ini under, inside hostConfigDir.
const appINIFilename = "app.ini"

// readyTimeout bounds how long Up waits for the forgejo container to
// accept `docker compose exec` after Converge starts it.
const readyTimeout = 60 * time.Second

// readyInterval is the delay between readiness probes within readyTimeout.
// A var, not a const, so tests can shorten it rather than actually waiting.
var readyInterval = 2 * time.Second

// Host is everything Up needs from a connected SSH session: orchestrate.
// Transport (so Up can hand it straight to orchestrate.Converge) plus Run
// and CheckHost. *orchestrate.Client (ORCH-001) satisfies it in
// production; tests use a fake, so Up's sequencing can be asserted without
// a real SSH server. Up does not connect host or call its Close — that is
// the caller's job, symmetric with orchestrate.Converge taking an
// already-reachable Transport rather than connecting one itself.
type Host interface {
	orchestrate.Transport
	Run(ctx context.Context, command string, stdout, stderr io.Writer) error
	CheckHost(ctx context.Context) error
}

// Options configures Up beyond the target host and bundle.
type Options struct {
	// RemoteDir is the directory on the host Up deploys into: Compose
	// files, and the rendered forge config, live under it. Required.
	RemoteDir string
}

// Up deploys b's full stateless layer to host (UP-001): it verifies Docker
// is reachable, resolves the bundle's key material and renders and ships
// Forgejo's app.ini, converges the host to the bundle's Compose definition
// plus that config, waits for Forgejo to accept commands, and provisions
// the first admin account.
//
// Up owns job's terminal event: it calls job.Succeeded or job.Failed
// exactly once, after every step below has run (or the first one fails),
// and returns the same error it reported through job.
func Up(ctx context.Context, job *events.Job, host Host, b *bundle.Bundle, opts Options) error {
	if err := up(ctx, job, host, b, opts); err != nil {
		job.Failed(err.Error())
		return err
	}
	job.Succeeded("forge deployed")
	return nil
}

func up(ctx context.Context, job *events.Job, host Host, b *bundle.Bundle, opts Options) error {
	if b == nil {
		return fmt.Errorf("deploy: bundle is required")
	}
	if strings.TrimSpace(opts.RemoteDir) == "" {
		return fmt.Errorf("deploy: remote directory is required")
	}

	job.Started(StepCheckHost, "checking Docker is reachable")
	if err := host.CheckHost(ctx); err != nil {
		job.Emit(StepCheckHost, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: check host: %w", err)
	}
	job.Emit(StepCheckHost, events.StateSucceeded, "Docker reachable")

	job.Started(StepConfigureForge, "resolving key material and rendering app.ini")
	compose, err := configureForge(ctx, host, b, opts.RemoteDir)
	if err != nil {
		job.Emit(StepConfigureForge, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: configure forge: %w", err)
	}
	job.Emit(StepConfigureForge, events.StateSucceeded, "app.ini rendered and shipped")

	job.Started(StepConverge, "converging host to bundle definition")
	deployed := &bundle.Bundle{Manifest: b.Manifest, Compose: compose}
	if err := orchestrate.Converge(ctx, host, opts.RemoteDir, deployed); err != nil {
		job.Emit(StepConverge, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: converge: %w", err)
	}
	job.Emit(StepConverge, events.StateSucceeded, "host converged")

	runner := &composeRunner{host: host, remoteDir: opts.RemoteDir, bundle: deployed}

	job.Started(StepWaitForge, "waiting for forgejo to accept commands")
	if err := waitReady(ctx, runner); err != nil {
		job.Emit(StepWaitForge, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: wait for forgejo: %w", err)
	}
	job.Emit(StepWaitForge, events.StateSucceeded, "forgejo ready")

	account, err := forge.NewAdminAccount(b.Manifest.Domain)
	if err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	if err := forge.Bootstrap(ctx, runner, job, account); err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	return nil
}

// configureForge resolves the bundle's key material, renders app.ini,
// ships it to host, and returns b's Compose files with a bind mount added
// so the forgejo service loads the file that was just shipped.
func configureForge(ctx context.Context, host Host, b *bundle.Bundle, remoteDir string) (map[string][]byte, error) {
	driver, err := keystore.New(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		return nil, fmt.Errorf("keystore driver: %w", err)
	}
	secrets, err := forge.ResolveSecrets(ctx, driver)
	if err != nil {
		return nil, fmt.Errorf("resolve key material: %w", err)
	}
	appINI, err := forge.RenderAppINI(&b.Manifest, secrets)
	if err != nil {
		return nil, fmt.Errorf("render app.ini: %w", err)
	}

	hostPath := path.Join(remoteDir, hostConfigDir, appINIFilename)
	if err := host.WriteFile(ctx, hostPath, appINI, 0o600); err != nil {
		return nil, fmt.Errorf("ship app.ini: %w", err)
	}

	compose, err := orchestrate.WithBindMount(b.Compose, forge.Service, hostPath, forge.AppINIPath)
	if err != nil {
		return nil, fmt.Errorf("mount app.ini: %w", err)
	}
	return compose, nil
}

// waitReady polls until the forgejo service accepts `docker compose exec`,
// or readyTimeout elapses. Converge's `docker compose up -d` returns once
// containers are created and started, which can race the container's own
// entrypoint init; admin bootstrap right after would then fail
// intermittently rather than deterministically, so this waits the race out
// instead of leaving it as a heisenbug in Bootstrap.
func waitReady(ctx context.Context, runner forge.Runner) error {
	deadline := time.Now().Add(readyTimeout)
	var lastErr error
	for {
		var stderr bytes.Buffer
		err := runner.Run(ctx, fmt.Sprintf("docker compose exec -T %s true", forge.Service), io.Discard, &stderr)
		if err == nil {
			return nil
		}
		lastErr = err
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			lastErr = fmt.Errorf("%s", msg)
		}

		if !time.Now().Before(deadline) {
			return fmt.Errorf("forgejo did not become ready within %s: %w", readyTimeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyInterval):
		}
	}
}
