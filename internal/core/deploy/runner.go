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
// every deploy, the credentials file the runner derives from it, which is
// reproducible from that secret alone, and the tool cache
// (runnerToolCacheDir), which is rebuildable from the network. Nothing
// precious lands here, so
// the four-kind state model (spec.md "The four kinds of state") gains no
// fifth member and backup captures nothing new — the registration itself
// lives in Forgejo's database (FAIL-005).
const runnerHostDir = "runner"

// runnerToolCacheDir is the directory under runnerHostDir holding the
// runner tool cache — the toolchains `actions/setup-node` and its siblings
// download, kept across jobs instead of re-fetched by every one
// (forge.RunnerToolCacheDir).
//
// It sits here, beside the runner's other deploy-time content, rather than
// under stateDir, and that placement is the point rather than an accident.
// Backup captures git out of <RemoteDir>/state/git, the database through
// the forgejo container, blobs through the blob driver, and key material
// through the keystore — nothing walks the remote directory looking for
// files — so a cache under this directory is captured by no exporter. That
// is what it deserves: it is rebuildable from the network, it grows without
// bound, and a snapshot is for the state that cannot be rebuilt (spec.md
// "The four kinds of state"). Putting it under stateDir would have grown
// every snapshot by a toolchain.
//
// Down removes the whole remote directory, so tearing a deployment down —
// or finishing a drill — discards the cache with it. That is correct: the
// next deployment rebuilds it on its first job, at the cost the first job
// on any fresh instance already pays.
//
// Nothing prunes it. A cache that keeps every toolchain version a workflow
// has ever asked for is the trade for never re-downloading one, and
// docs/using.md tells the operator so, since the disk it grows on is
// theirs.
const runnerToolCacheDir = "toolcache"

// RunnerSecretPath is the host-side path `up` writes the runner secret to,
// 0600. Exported for the same reason GitStatePath and friends are: it is a
// layout decision, and one spelling of it beats several.
func RunnerSecretPath(remoteDir string) string {
	return path.Join(remoteDir, runnerHostDir, forge.RunnerSecretFilename)
}

// RunnerConfigPath is the host-side path `up` writes the runner's
// configuration file to. It sits next to the secret in the mounted data
// directory so the daemon's -c flag finds it without a second mount.
func RunnerConfigPath(remoteDir string) string {
	return path.Join(remoteDir, runnerHostDir, forge.RunnerConfigFilename)
}

// RunnerHostPath is the host-side directory mounted into the runner as its
// data directory. RunnerSecretPath and RunnerConfigPath sit inside it.
func RunnerHostPath(remoteDir string) string {
	return path.Join(remoteDir, runnerHostDir)
}

// RunnerToolCachePath is the host-side directory job containers mount at
// forge.RunnerToolCacheDir.
//
// It is a host path and has to be: the runner reaches the host's Docker
// daemon over the mounted socket, so the daemon creating a job container
// resolves this source in the host's filesystem. The runner's own view of
// the same directory — inside RunnerDataDir, which this sits under — is a
// container path the daemon has never heard of, and handing it over
// produces a mount of the wrong directory rather than an error. Exported
// for the same reason GitStatePath and friends are: one spelling of a
// layout decision beats several.
func RunnerToolCachePath(remoteDir string) string {
	return path.Join(remoteDir, runnerHostDir, runnerToolCacheDir)
}

// configureRunner shapes the colocated Forgejo Actions runner's Compose
// service (FORGE-005) and returns compose with the change, plus whether the
// runner is being deployed at all — which is what tells the caller whether
// to run the registration step later, once Forgejo is up.
//
// When the bundle wants the runner (bundle.Manifest.ColocatedRunnerEnabled),
// it resolves the runner secret from the keystore, ships it to the host
// 0600 alongside the runner configuration that declares its labels and the
// tool cache its job containers mount, creates that cache directory, and
// layers onto the rendered service: the host directory holding those
// files, the host's Docker socket, DOCKER_HOST, the user to run as, and
// the command that derives credentials from the secret and starts the
// daemon against that configuration.
//
// The job image and the tool cache path are the two things that
// configuration is not free to invent: the image is the manifest's
// (ActionsJobImageOrDefault) and the path is the host's, since the job
// containers that mount it are started on the host's Docker daemon.
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

	if !colocatedRunnerPlanned(&b.Manifest) {
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
	toolCachePath := RunnerToolCachePath(remoteDir)
	if err := ensureToolCacheDir(ctx, host, toolCachePath); err != nil {
		return nil, false, fmt.Errorf("create runner tool cache directory: %w", err)
	}
	config := forge.RenderRunnerConfig(b.Manifest.ActionsJobImageOrDefault(), toolCachePath)
	if err := host.WriteFile(ctx, RunnerConfigPath(remoteDir), config, 0o644); err != nil {
		return nil, false, fmt.Errorf("ship runner config: %w", err)
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

// ensureToolCacheDir creates the runner tool cache directory on the host
// and makes it writable by whoever a job container turns out to run as.
//
// It has to exist before the first job: Docker creates a missing bind
// source itself, but as root and owned by root, which is the one case a job
// image running as a non-root user cannot then write to — and a tool cache
// nobody can write to is the slow path this whole directory exists to end,
// arriving silently as "setup-node downloaded again."
//
// The mode is deliberately permissive, for the same reason. Job containers
// run as whatever their image declares, Farrier does not choose the image
// (bundle.ActionsConfig.JobImage) and cannot know the uid, and the cache is
// worthless unless every one of them can write it. It concedes nothing:
// reaching this directory at all means running a job on this runner, and a
// job on this runner already holds the host's Docker socket — the whole
// host, not one rebuildable directory (spec.md "CI trust boundary").
//
// Creating it is idempotent, so a repeated `up` leaves an existing cache
// and its contents untouched (UP-003).
func ensureToolCacheDir(ctx context.Context, host Host, toolCachePath string) error {
	if err := ensureDirs(ctx, host, toolCachePath); err != nil {
		return err
	}
	_, err := host.Output(ctx, fmt.Sprintf("chmod 0777 %s", stateShQuote(toolCachePath)))
	return err
}

// colocatedRunnerPlanned reports whether this deployment will carry a
// colocated runner: the manifest wants one (FORGE-005) and pins an image to
// run it from. Both halves are configureRunner's own gate above, and this
// is the same question asked before the deployment touches the host —
// checkRunnerReachableAddress needs it to know whether a loopback address
// costs this instance its CI.
//
// The contradiction configureRunner fails on — a manifest that asks for the
// runner and pins nothing — is deliberately not this function's business.
// It reads as "no runner planned" here, and the deployment still fails at
// the step that owns that error, with the message that names the fix.
func colocatedRunnerPlanned(m *bundle.Manifest) bool {
	if !m.ColocatedRunnerEnabled() {
		return false
	}
	_, pinned := m.Images[forge.RunnerService]
	return pinned
}

// runnerInstanceURL is the URL the colocated runner registers against: the
// instance's public URL (forge.InstanceURL) in an ordinary deployment, and
// the bundle domain at Caddy's *container* port under quarantine.
//
// The difference is what resolves the domain. Ordinarily the runner's job
// containers and the runner itself reach the instance the way any other
// client does — public DNS to the host, then the published web port, or the
// proxy in front of it — so the public URL is exactly right. It has to be:
// this URL is not only the daemon's own connection but the server URL the
// daemon hands each job container to clone from, so an endpoint only the
// runner can resolve would break CI rather than fix it (forge.InstanceURL).
// Under
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
