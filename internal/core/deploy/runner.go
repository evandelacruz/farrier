package deploy

import (
	"context"
	"fmt"
	"path"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/caddy"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// runnerHostDir is the directory under RemoteDir the colocated Actions
// runner's deploy-time content lives in, mounted into the runner container
// as its data directory.
//
// It sits alongside hostConfigDir rather than under stateDir on purpose: it
// holds the runner's secret, which is bundle key material resolved fresh on
// every deploy, and the credentials file the runner derives from it, which
// is reproducible from that secret alone. Nothing precious lands here, so
// the four-kind state model (spec.md "The four kinds of state") gains no
// fifth member and backup captures nothing new — the registration itself
// lives in Forgejo's database (FAIL-005).
const runnerHostDir = "runner"

// RunnerSecretPath is the host-side path `up` writes the runner secret to,
// 0600. Exported for the same reason GitStatePath and friends are: it is a
// layout decision, and one spelling of it beats several.
func RunnerSecretPath(remoteDir string) string {
	return path.Join(remoteDir, runnerHostDir, forge.RunnerSecretFilename)
}

// RunnerHostPath is the host-side directory mounted into the runner as its
// data directory. RunnerSecretPath sits inside it.
func RunnerHostPath(remoteDir string) string {
	return path.Join(remoteDir, runnerHostDir)
}

// configureRunner shapes the colocated Forgejo Actions runner's Compose
// service (FORGE-005) and returns compose with the change, plus whether the
// runner is being deployed at all — which is what tells the caller whether
// to run the registration step later, once Forgejo is up.
//
// When the bundle wants the runner (bundle.Manifest.ColocatedRunnerEnabled),
// it resolves the runner secret from the keystore, ships it to the host
// 0600, and layers onto the rendered service: the host directory holding
// that secret, the host's Docker socket, DOCKER_HOST, the user to run as,
// and the command that derives credentials from the secret and starts the
// daemon.
//
// Mounting the Docker socket is what lets the runner start job containers,
// and is the trade spec.md "CI trust boundary" > "The colocated runner holds
// the host's Docker socket" records and accepts. The container runs as root
// because that socket is root-owned and the runner has to reach it; the
// alternatives — privileged or rootless Docker-in-Docker — were considered
// and rejected in that same section, so this does not pretend to be less
// than it is.
//
// When the bundle does not want the runner, the service is removed from the
// Compose definition entirely, so Converge's --remove-orphans takes down a
// runner an earlier deploy started (orchestrate.WithoutService). Nothing is
// deregistered: the registration is a database row the operator can see and
// delete in Forgejo's admin UI, and removing it here would mean a deployment
// deleting instance state on a manifest edit.
//
// A manifest that pins no runner image is the one case that depends on how
// the preference was expressed. Asking for the runner explicitly and pinning
// no image is a contradiction, and fails; leaving the preference unset on a
// bundle that predates the field is not, and skips the runner with an event
// saying so rather than failing a deployment that used to work.
//
// address is the operator-supplied address a nameless bundle is served at
// (UP-006) and is empty for a named one; it is what the runner is pointed
// at, through runnerInstanceURL, so a nameless instance's CI reaches the
// instance over plain HTTP at the same URL the operator's browser does.
//
// Every part of this is safe to repeat (UP-003): the secret is non-rotating
// key material written back byte-for-byte, the mounts and command are
// derived from the manifest alone, and the registration the caller performs
// afterwards is an upsert keyed by that same secret.
func configureRunner(ctx context.Context, host Host, b *bundle.Bundle, remoteDir, address string, compose map[string][]byte, quarantine bool) (map[string][]byte, bool, error) {
	if !b.Manifest.ColocatedRunnerEnabled() {
		compose, err := orchestrate.WithoutService(compose, forge.RunnerService)
		if err != nil {
			return nil, false, fmt.Errorf("remove colocated runner: %w", err)
		}
		return compose, false, nil
	}

	if _, ok := b.Manifest.Images[forge.RunnerService]; !ok {
		if b.Manifest.ColocatedRunnerDeclared() {
			return nil, false, fmt.Errorf(
				"manifest asks for a colocated runner but pins no %q image; pin one or set actions.colocatedRunner to false",
				forge.RunnerService,
			)
		}
		return compose, false, nil
	}

	secret, err := resolveRunnerSecret(ctx, b)
	if err != nil {
		return nil, false, err
	}

	secretPath := RunnerSecretPath(remoteDir)
	if err := host.WriteFile(ctx, secretPath, []byte(secret), 0o600); err != nil {
		return nil, false, fmt.Errorf("ship runner secret: %w", err)
	}

	compose, err = orchestrate.WithBindMount(compose, forge.RunnerService, RunnerHostPath(remoteDir), forge.RunnerDataDir)
	if err != nil {
		return nil, false, fmt.Errorf("mount runner data directory: %w", err)
	}
	compose, err = orchestrate.WithBindMount(compose, forge.RunnerService, forge.DockerSocketPath, forge.DockerSocketPath)
	if err != nil {
		return nil, false, fmt.Errorf("mount docker socket: %w", err)
	}
	compose, err = orchestrate.WithEnv(compose, forge.RunnerService, forge.DockerHostEnv, forge.DockerHostValue)
	if err != nil {
		return nil, false, fmt.Errorf("set runner docker host: %w", err)
	}
	compose, err = orchestrate.WithUser(compose, forge.RunnerService, forge.RunnerUser)
	if err != nil {
		return nil, false, fmt.Errorf("set runner user: %w", err)
	}
	compose, err = orchestrate.WithCommand(compose, forge.RunnerService, forge.RunnerCommand(runnerInstanceURL(&b.Manifest, address, quarantine)))
	if err != nil {
		return nil, false, fmt.Errorf("set runner command: %w", err)
	}
	return compose, true, nil
}

// runnerInstanceURL is the URL the colocated runner registers against: the
// instance's public URL (forge.InstanceURL) in an ordinary deployment, and
// the bundle domain at Caddy's *container* port under quarantine.
//
// The difference is what resolves the domain. Ordinarily the runner's job
// containers and the runner itself reach the instance the way any other
// client does — public DNS to the host, then the published web port, or the
// proxy in front of it — so the public URL is exactly right. Under
// quarantine, configureTLS gives Caddy the bundle domain as a Docker
// network alias (DRIL-002) so the drilled runner reaches the drilled
// instance rather than production, and that alias resolves to the container
// itself, where no host-side port mapping applies. A drilled instance
// published on a non-standard host port would otherwise send its runner to
// a port nothing inside the network is listening on.
func runnerInstanceURL(m *bundle.Manifest, address string, quarantine bool) string {
	if quarantine {
		return m.WebURL(m.Domain, caddy.HTTPSPort)
	}
	return forge.InstanceURL(m, address)
}

// resolveRunnerSecret reads the bundle's runner secret through its keystore
// driver and checks its shape before it reaches a host. The value is
// returned, never logged or emitted: it is key material, and the only two
// places it is allowed to land are a 0600 file on the deploy host and the
// stdin of Forgejo's registration command (KEY-003).
func resolveRunnerSecret(ctx context.Context, b *bundle.Bundle) (string, error) {
	driver, err := keystore.New(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		return "", fmt.Errorf("keystore driver: %w", err)
	}
	stored, err := driver.Resolve(ctx, forge.KeyRunnerSecret)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", forge.KeyRunnerSecret, err)
	}
	secret := stored.Reveal()
	if err := forge.ValidateRunnerSecret(secret); err != nil {
		return "", err
	}
	return secret, nil
}
