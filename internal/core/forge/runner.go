package forge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

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

// RunnerConfigFilename is the name, inside the same mounted directory, of
// the runner's configuration file. Farrier ships it so the colocated runner
// declares its labels on every connect — Forgejo takes labels from the
// runner, not the other way around — rather than leaving the image's empty
// default and waiting forever for a matching job (FORGE-005). See
// RunnerLabelNames for why registration declares the same set.
const RunnerConfigFilename = "config.yaml"

// RunnerToolCacheDir is the directory job containers find the runner tool
// cache at. It is the path the `setup-*` actions read out of
// RUNNER_TOOL_CACHE, and /opt/hostedtoolcache is that variable's
// conventional value — the one GitHub's hosted runners ship preloaded, and
// the one those actions fall back to when nothing sets it.
//
// It matters because actions/setup-node, setup-python, setup-go, and
// setup-java do not look at what is already on PATH. They look here, and
// download the version they were asked for when it is absent. A job
// container is fresh every run, so with nothing mounted at this path every
// run re-downloads a toolchain — minutes per job, on every job, for the
// life of the instance. Mounting one host directory here is what turns that
// into a first-run cost (deploy.RunnerToolCachePath).
const RunnerToolCacheDir = "/opt/hostedtoolcache"

// RunnerLabelNames are the runs-on values the colocated runner answers to.
// docker is Forgejo's conventional label; ubuntu-latest is the GitHub
// habit, aliased onto the same container image so existing workflows
// land.
//
// Two places declare these, and both are load-bearing — deleting either
// one leaves a real gap:
//
//   - RenderRunnerConfig writes them into the runner's own configuration
//     file, and the daemon declares them to Forgejo on every connect.
//     This is the authoritative copy: Forgejo overwrites the runner's
//     labels with whatever the daemon declares, from the first connect
//     onward, so the config file is what actually makes a job schedule.
//   - registerRunnerCommand passes them to `forgejo-cli actions register`,
//     which writes them onto the runner row at registration — before the
//     daemon has connected at all. Without it, the row Forgejo creates
//     carries a single empty label, and an operator looking at the admin
//     runner list in the window before the first connect sees a runner
//     that answers to nothing. That unreadable state is the symptom this
//     whole mechanism exists to end, so it is worth closing too.
//
// They cannot drift: both read this slice.
var RunnerLabelNames = []string{"docker", "ubuntu-latest"}

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

// runnerConfigFile is the subset of the forgejo-runner configuration file
// Farrier writes. The field names and YAML keys are the runner's own
// (code.forgejo.org/forgejo/runner, internal/pkg/config): `runner.labels`,
// `container.options`, `container.valid_volumes`. Marshalling a struct
// rather than printing lines is what keeps an operator-supplied job image
// or remote directory from having to be escaped by hand into valid YAML.
//
// Container is a pointer so a configuration with nothing to mount omits the
// section entirely rather than writing an empty one.
type runnerConfigFile struct {
	Runner    runnerConfigRunner     `yaml:"runner"`
	Container *runnerConfigContainer `yaml:"container,omitempty"`
}

type runnerConfigRunner struct {
	Labels []string `yaml:"labels"`
}

type runnerConfigContainer struct {
	Options      string   `yaml:"options"`
	ValidVolumes []string `yaml:"valid_volumes"`
}

// RenderRunnerConfig renders the colocated runner's configuration file
// (FORGE-005): the labels it declares when it connects to Forgejo, each
// mapped to jobImage, and the tool cache every job container mounts.
//
// Labels are runner-side configuration. Forgejo updates its view of them
// every time the runner establishes a connection, so shipping this file
// and pointing the daemon at it is what makes a fresh `up` answer
// runs-on: ubuntu-latest without operator action. The file carries no
// secrets — the registration secret stays in RunnerSecretFilename.
//
// toolCacheHostPath is the tool cache's directory **on the host**, not
// inside the runner container. The runner starts job containers on the
// host's Docker daemon (DockerHostValue), so the daemon resolves this
// source path in the host's own filesystem — the runner's view of the same
// directory, under RunnerDataDir, means nothing to it. Empty renders no
// container section at all, which is the shape a caller with no cache to
// mount gets rather than a half-written mount.
//
// Two keys are needed, not one. `container.options` is the mount; the
// runner also sanitizes every job container's binds against
// `container.valid_volumes`, a glob allowlist it always applies, so a
// source missing from that list is dropped with a warning in the job log
// and the job runs on with no cache and no failure. Both are set from the
// same string so they cannot disagree.
func RenderRunnerConfig(jobImage, toolCacheHostPath string) []byte {
	config := runnerConfigFile{}
	for _, name := range RunnerLabelNames {
		config.Runner.Labels = append(config.Runner.Labels, fmt.Sprintf("%s:docker://%s", name, jobImage))
	}
	if strings.TrimSpace(toolCacheHostPath) != "" {
		config.Container = &runnerConfigContainer{
			// Shell-quoted: the runner splits this string with POSIX
			// shell quoting before handing it to Docker's own flag
			// parser, and the host path descends from the operator's
			// remote directory, which may carry a space.
			Options:      "--volume " + quote(toolCacheHostPath+":"+RunnerToolCacheDir),
			ValidVolumes: []string{toolCacheHostPath},
		}
	}

	body, err := yaml.Marshal(config)
	if err != nil {
		// Unreachable: every field is a string or a slice of strings.
		// Panicking beats returning a second value every caller would
		// have to invent an error path for.
		panic(fmt.Sprintf("forge: render runner config: %v", err))
	}

	var b strings.Builder
	b.WriteString("# Generated by Farrier. Labels are how workflows find this runner.\n")
	b.Write(body)
	return []byte(b.String())
}

// RunnerCommand is the command the colocated runner container runs
// (FORGE-005): derive its credentials from the mounted secret if it has
// none yet, then run the daemon against the mounted configuration file.
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
//
// -c is a root-level flag on forgejo-runner, so every subcommand takes it,
// and both subcommands here use it for labels. create-runner-file copies
// the configured labels into the `.runner` credentials file it writes; the
// daemon declares them on each connect and they take precedence over
// whatever `.runner` holds. Passing it to both means the labels are right
// from the moment the credentials file exists, not only once the daemon
// has come up.
func RunnerCommand(instanceURL string) []string {
	configFlag := "-c " + quote(RunnerConfigFilename)
	script := strings.Join([]string{
		"cd " + quote(RunnerDataDir),
		"if [ ! -f .runner ]; then forgejo-runner create-runner-file " + configFlag +
			" --instance " + quote(instanceURL) +
			" --secret \"$(cat " + quote(RunnerSecretFilename) + ")\"; fi",
		"exec forgejo-runner daemon " + configFlag,
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
//
// --labels seeds the runner row's labels at registration. Forgejo splits
// the value on commas and stores it, so omitting the flag stores one empty
// label rather than none — the blank label set an operator sees in the
// admin runner list before the daemon's first connect. The daemon's own
// declaration (RunnerLabelNames) is authoritative from that connect
// onward; this only covers the window before it.
func registerRunnerCommand(secretPath string) string {
	return fmt.Sprintf(
		"docker compose exec -T -u %s %s forgejo forgejo-cli actions register --secret-stdin --name %s --labels %s < %s",
		runUser, Service, quote(RunnerName), quote(strings.Join(RunnerLabelNames, ",")), quote(secretPath),
	)
}
