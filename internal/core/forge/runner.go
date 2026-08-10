package forge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/events"
)

// RunnerService is the Compose service name — and therefore the manifest
// image component name, since orchestrate.Render keys one service per image
// component — of the colocated Forgejo Actions runner (FORGE-005).
//
// It is named for the role, not the product, unlike Service ("forgejo") and
// caddy.Service: the runner is released on its own cadence from its own
// repository (code.forgejo.org/forgejo/runner), and spec.md calls it "the
// runner" throughout. Keying the manifest by role means an upstream rename
// changes a pinned image reference rather than the bundle format.
const RunnerService = "runner"

// StepRunnerRegister identifies the runner-registration step in a
// deployment job's event stream.
const StepRunnerRegister = "runner-register"

// KeyRunnerSecret names the bundle key material that binds the colocated
// runner to its registration on the instance. Like Forgejo's own three
// secrets it is generated once at init (INIT-003) and carried through every
// backup and restore; this package only names and uses it.
const KeyRunnerSecret = "forgejo_runner_secret"

// runnerSecretBytes is the random byte count behind a runner secret. The
// secret is a 40-character lowercase hex string: Forgejo reads the first 16
// characters as the runner's identifier and the remaining 24 as the secret
// proper, so the length is fixed by Forgejo's format, not chosen here.
const runnerSecretBytes = 20

// RunnerName is the name the colocated runner registers under, shown to
// operators in Forgejo's admin UI. It is fixed rather than derived from the
// domain or the host: registration is keyed by the secret's identifier, so
// the name is a label, and a stable one keeps a re-registration recognizable
// as the same runner rather than a rename.
const RunnerName = "farrier-colocated"

// RunnerDataDir is the container-side directory the runner keeps its
// generated `.runner` credentials file in, and the directory the deploying
// side mounts the runner's secret into. The official runner image's own
// documented layout uses /data for exactly this. Exported so the caller
// wiring the bind mount (deploy.configureRunner) targets the same path
// RunnerCommand works in.
const RunnerDataDir = "/data"

// RunnerUser is the user the runner container runs as, in Compose's
// "uid:gid" form: root, because DockerSocketPath is root-owned and the
// runner has to reach it to start job containers. Stated explicitly rather
// than left to the image's default, since it is the concrete shape of the
// trade DockerSocketPath describes.
const RunnerUser = "0:0"

// RunnerSecretFilename is the name, inside the host directory mounted at
// runnerDataDir, of the file holding the runner secret. The deploying side
// (deploy.configureRunner) writes it 0600 on the host; the runner reads it
// at start to derive its credentials, so the secret never has to be an
// argument on a command line or an environment variable in the Compose
// definition (KEY-003).
const RunnerSecretFilename = "secret"

// DockerSocketPath is the host's Docker socket, mounted into the runner so
// it can start job containers.
//
// This is the trade spec.md "CI trust boundary" > "The colocated runner
// holds the host's Docker socket" records and accepts: Actions runs every
// job step in a container the workflow names, a container cannot create
// containers on its own, and anything that can reach this socket can start
// any container — including one mounting the whole host filesystem. The
// escape hatch is topology, not configuration: an operator who does not
// want that on the forge host sets actions.colocatedRunner to false in the
// manifest and registers a remote runner instead.
const DockerSocketPath = "/var/run/docker.sock"

// DockerHostEnv and DockerHostValue point the runner at the socket above.
// The value is the runner's own default; set explicitly so the Compose
// definition states which Docker daemon runs job containers rather than
// leaving it to be inferred from the mount.
const (
	DockerHostEnv   = "DOCKER_HOST"
	DockerHostValue = "unix://" + DockerSocketPath
)

// runnerAlreadyRegisteredMarker is the substring Forgejo's registration
// rejects a duplicate with. Registration by secret is an upsert — that is
// the whole point of the offline form, and what makes a repeated `up` safe
// (UP-003) — but a Forgejo version that reports the existing registration as
// an error instead of updating it must not fail a deployment either.
const runnerAlreadyRegisteredMarker = "already exists"

// NewRunnerSecret generates a fresh runner secret: 40 lowercase hex
// characters, the format Forgejo's offline runner registration requires.
func NewRunnerSecret() (string, error) {
	buf := make([]byte, runnerSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("forge: generate runner secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ValidateRunnerSecret checks that secret has the shape Forgejo's offline
// registration accepts, so a malformed one is caught before it reaches a
// host rather than surfacing as an opaque CLI failure inside a container.
func ValidateRunnerSecret(secret string) error {
	if len(secret) != runnerSecretBytes*2 {
		return fmt.Errorf("forge: runner secret must be %d hexadecimal characters", runnerSecretBytes*2)
	}
	if _, err := hex.DecodeString(secret); err != nil {
		return fmt.Errorf("forge: runner secret must be hexadecimal: %w", err)
	}
	if strings.ToLower(secret) != secret {
		return fmt.Errorf("forge: runner secret must be lowercase hexadecimal")
	}
	return nil
}

// RunnerCommand is the command the colocated runner container runs
// (FORGE-005): derive its credentials from the mounted secret if it has
// none yet, then run the daemon.
//
// It is list form, not a shell string, so Compose passes the script to
// `sh -ec` verbatim instead of word-splitting it. The credentials file is
// derived from the secret and nothing else, so it is reproducible rather
// than precious: a container recreated on a host with an empty data
// directory rebuilds the identical file and reconnects to the same
// registration. That is what keeps the runner on the stateless side of
// spec.md "Stateless vs. stateful" — the registration itself lives in
// Forgejo's database (FAIL-005), never here.
//
// The daemon is started with exec so it becomes PID 1 and receives Compose's
// stop signal directly. Nothing in the script waits for the instance: the
// daemon retries its connection, and the service's restart policy covers the
// window between `docker compose up -d` starting this container and
// RegisterRunner creating its registration.
func RunnerCommand(instanceURL string) []string {
	script := strings.Join([]string{
		"cd " + quote(RunnerDataDir),
		"if [ ! -f .runner ]; then forgejo-runner create-runner-file --instance " +
			quote(instanceURL) + " --secret \"$(cat " + quote(RunnerSecretFilename) + ")\"; fi",
		"exec forgejo-runner daemon",
	}, "\n")
	return []string{"sh", "-ec", script}
}

// RegisterRunner registers the colocated runner against the running
// instance without operator action (FORGE-005), reading the secret from
// secretPath on the deploy host and feeding it to Forgejo's registration
// CLI on stdin.
//
// Registration is keyed by the secret's identifier, so running it again
// updates the existing registration instead of creating a second one — the
// property UP-003 needs, and the reason `up` can register unconditionally
// rather than reading the instance back first to decide. It is also why a
// restored instance is safe: the secret is bundle key material carried
// through every backup and restore, so the registration a snapshot already
// contains is the one this call updates, at whatever Forgejo version the
// snapshot pinned.
//
// The secret is never an argument: it arrives on the command's stdin from a
// file the deploying side wrote 0600, so it stays out of the command line,
// the host's process list, and any error text quoting the command (KEY-003).
// RegisterRunner does not emit a job-terminal event; the caller's deployment
// flow owns that.
func RegisterRunner(ctx context.Context, runner Runner, job *events.Job, secretPath string) error {
	job.Started(StepRunnerRegister, "registering the colocated actions runner")

	if strings.TrimSpace(secretPath) == "" {
		const detail = "runner secret path is required"
		job.Emit(StepRunnerRegister, events.StateFailed, detail)
		return fmt.Errorf("forge: register runner: %s", detail)
	}

	var stdout, stderr bytes.Buffer
	if err := runner.Run(ctx, registerRunnerCommand(secretPath), &stdout, &stderr); err != nil {
		// err.Error() is deliberately unused when the command said anything
		// itself: the transport embeds the whole command in its error text,
		// and while the secret is not in that command, the redirection names
		// the path it lives at — no reason to widen what a failure prints.
		// Both output streams are read, since Forgejo's CLI does not commit
		// to writing a fatal to stderr.
		detail := FailureDetail(&stdout, &stderr)
		if strings.Contains(detail, runnerAlreadyRegisteredMarker) {
			job.Emit(StepRunnerRegister, events.StateSucceeded, fmt.Sprintf(
				"runner %s is already registered, leaving it as-is", RunnerName,
			))
			return nil
		}
		job.Emit(StepRunnerRegister, events.StateFailed, fmt.Sprintf("register runner: %s", detail))
		return fmt.Errorf("forge: register runner: %s", detail)
	}

	job.Emit(StepRunnerRegister, events.StateSucceeded, fmt.Sprintf(
		"runner %s registered against the instance", RunnerName,
	))
	return nil
}

// registerRunnerCommand builds the `docker compose exec` invocation that
// registers the runner from inside the forgejo container.
//
// The secret arrives by redirecting secretPath into the command's stdin
// rather than through a pipe: a caller prefixes this with the deployment's
// Compose project reference (deploy.composeRunner), which sets environment
// variables for the command it precedes, and a pipeline would leave
// everything right of the pipe without them.
//
// It runs as the git user, the form Forgejo documents for its admin CLI
// under Docker, so the command touches the database as the user that owns
// it.
func registerRunnerCommand(secretPath string) string {
	return fmt.Sprintf(
		"docker compose exec -T -u %s %s forgejo forgejo-cli actions register --secret-stdin --name %s < %s",
		runUser, Service, quote(RunnerName), quote(secretPath),
	)
}
