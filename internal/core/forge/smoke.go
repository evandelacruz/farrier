package forge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/events"
)

// StepSmokeCI identifies the smoke-CI step in a job's event stream.
const StepSmokeCI = "smoke-ci"

// smokeWorkflowPath is where the smoke workflow is committed. Forgejo reads
// workflows from .forgejo/workflows and .github/workflows alike; the native
// directory is used so the scratch repository says plainly which forge it
// was written for.
const smokeWorkflowPath = ".forgejo/workflows/smoke.yml"

// smokeRunsOn is the runner label the smoke job asks for. `docker` is the
// colocated runner's default label — the one Forgejo's own Actions
// documentation writes every example against — and FORGE-005 registers the
// runner with its defaults rather than overriding them. A drilled instance
// whose runner answers to something else leaves the run queued, which is
// reported as exactly that rather than as a broken restore.
const smokeRunsOn = "docker"

// smokeBranch is the branch the scratch repository is initialized on and the
// workflow is committed to. Named explicitly rather than left to the
// instance's default so the commit-status poll below has a ref it can count
// on, whatever the restored instance configures.
const smokeBranch = "main"

// defaultSmokeTimeout bounds how long SmokeCI waits for the run its commit
// triggers. It is generous because the first job on a freshly drilled host
// pulls its job-container image before it runs a single step, and a drill
// that gave up during that pull would report a failure the instance does not
// have.
const defaultSmokeTimeout = 10 * time.Minute

// defaultSmokePoll is how often the instance is asked for the run's outcome
// while waiting.
const defaultSmokePoll = 5 * time.Second

// SmokeOptions configures SmokeCI. Every field has a working default, so a
// caller that wants DRIL-001's smoke job and nothing more passes the zero
// value.
type SmokeOptions struct {
	// Repository is the name of the scratch repository to create. Empty
	// generates one with a random suffix, which is what callers should do:
	// the drilled instance holds production's repositories, and a fixed name
	// could collide with one of them or with a scratch repository an earlier
	// drill against the same target left behind.
	Repository string

	// Timeout bounds the wait for the run to finish. Zero uses
	// defaultSmokeTimeout.
	Timeout time.Duration

	// Poll is the interval between checks on the run's outcome. Zero uses
	// defaultSmokePoll.
	Poll time.Duration
}

func (o SmokeOptions) timeout() time.Duration {
	if o.Timeout <= 0 {
		return defaultSmokeTimeout
	}
	return o.Timeout
}

func (o SmokeOptions) poll() time.Duration {
	if o.Poll <= 0 {
		return defaultSmokePoll
	}
	return o.Poll
}

// SmokeResult reports what a smoke job left on the instance.
type SmokeResult struct {
	// Repository is the scratch repository the smoke job was run in, in
	// owner/name form. It is left in place: disposing of the drilled target
	// is DRIL-003's job, and it disposes of everything at once.
	Repository string
}

// SmokeCI runs DRIL-001's smoke CI job against a booted instance: it creates
// a scratch repository holding one trivial workflow, and the commit that
// adds that workflow is the push that dispatches it. SmokeCI then waits for
// the run to finish and reports success, or what specifically went wrong —
// the repository could not be created, the run failed, or nothing picked it
// up before the timeout.
//
// It emits StepSmokeCI started/succeeded/failed through job but never job's
// own terminal event, matching Bootstrap and RegisterRunner: the caller's
// flow owns that.
//
// # Why this runs inside the forgejo container
//
// Everything happens in one shell script passed to `docker compose exec`,
// the same path admin bootstrap and runner registration already use, and it
// talks to Forgejo over the loopback interface inside that container. That
// is what makes the smoke job work on a drilled instance without touching
// quarantine (DRIL-002): the instance publishes no routable port and the
// operator reaches it through an SSH tunnel, so Farrier drives it over the
// SSH session it already holds rather than opening a way in.
//
// It also keeps the API token the script mints inside the container. Farrier
// never receives it, never writes it to the host, and it appears in no
// event, no log, and no error text — the Go side builds a command containing
// no credential at all, so even an error that quotes the whole command
// leaks nothing. The token is left on the instance rather than revoked,
// alongside the scratch repository, because the drilled target is disposable
// and DRIL-003 disposes of it whole.
//
// # The one thing the caller must have arranged
//
// A runner able to accept the job. The colocated runner (FORGE-005) is that
// runner, and on a drilled instance it must resolve the bundle domain to the
// drilled Caddy rather than to production — see deploy.configureTLS's
// domain alias. Without it the run is created and never claimed, which
// SmokeCI reports as the run not starting.
func SmokeCI(ctx context.Context, runner Runner, job *events.Job, opts SmokeOptions) (SmokeResult, error) {
	job.Started(StepSmokeCI, "running a smoke CI job on the instance")

	repo := strings.TrimSpace(opts.Repository)
	if repo == "" {
		suffix, err := randomSuffix()
		if err != nil {
			job.Emit(StepSmokeCI, events.StateFailed, err.Error())
			return SmokeResult{}, fmt.Errorf("forge: smoke ci: %w", err)
		}
		repo = "farrier-drill-smoke-" + suffix
	}
	tokenSuffix, err := randomSuffix()
	if err != nil {
		job.Emit(StepSmokeCI, events.StateFailed, err.Error())
		return SmokeResult{}, fmt.Errorf("forge: smoke ci: %w", err)
	}

	var stdout, stderr bytes.Buffer
	command := smokeCommand(repo, "farrier-drill-smoke-"+tokenSuffix, opts.timeout(), opts.poll())
	if err := runner.Run(ctx, command, &stdout, &stderr); err != nil {
		// Both streams, for the same reason Bootstrap reads both: a drill
		// that reports "no output" tells the operator nothing about why the
		// instance it just restored cannot run a job. The script routes
		// every command's output through a substitution and writes its own
		// failures to stderr, so stdout is normally empty and lastLine lands
		// on the message naming what broke; when stdout does carry something
		// it came from the CLI aborting under the script, and failureDetail
		// puts it last, which is where lastLine looks.
		detail := lastLine(failureDetail(&stdout, &stderr))
		job.Emit(StepSmokeCI, events.StateFailed, fmt.Sprintf("smoke ci: %s", detail))
		return SmokeResult{}, fmt.Errorf("forge: smoke ci: %s", detail)
	}

	// The scratch repository belongs to the first admin account, because
	// that is whose token the script mints above — so its owner segment is
	// adminUsername, not a name of its own. Renaming that constant moves
	// this path, which is why forge_test pins the two together.
	result := SmokeResult{Repository: adminUsername + "/" + repo}
	job.Emit(StepSmokeCI, events.StateSucceeded, fmt.Sprintf(
		"smoke CI job succeeded: %s ran to completion in scratch repository %s",
		smokeWorkflowPath, result.Repository,
	))
	return result, nil
}

// randomSuffix returns eight hexadecimal characters, enough that a scratch
// repository or token name never collides with one an earlier drill against
// the same target left behind.
func randomSuffix() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate smoke job suffix: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// smokeWorkflow is the workflow the smoke job runs: one job, one step, no
// checkout and no actions to fetch. It is deliberately the smallest thing
// that still proves the whole chain — a push created a run, a runner claimed
// it, a job container started, a step ran, and the outcome came back — since
// anything more would be testing the workflow rather than the instance.
func smokeWorkflow() string {
	return strings.Join([]string{
		"name: farrier-drill-smoke",
		"on: [push]",
		"jobs:",
		"  smoke:",
		"    runs-on: " + smokeRunsOn,
		"    steps:",
		"      - run: echo farrier drill smoke job",
		"",
	}, "\n")
}

// smokeCommand builds the `docker compose exec` invocation that runs the
// whole smoke job inside the forgejo container.
//
// It runs as the git user, the form Forgejo documents for its admin CLI
// under Docker, and the script it carries is single-quoted as one argument
// so the remote shell hands it to `sh -ec` verbatim rather than expanding
// any of it on the way.
func smokeCommand(repo, tokenName string, timeout, poll time.Duration) string {
	return fmt.Sprintf(
		"docker compose exec -T -u %s %s sh -ec %s",
		runUser, Service, quote(smokeScript(repo, tokenName, timeout, poll)),
	)
}

// smokeScript is the smoke job itself, as a POSIX shell script the forgejo
// container runs.
//
// Every step reports what it was doing on stderr and exits non-zero, so a
// failure names which part of the smoke job broke rather than surfacing as
// a bare exit status. The script speaks to Forgejo's own HTTP API over the
// container's loopback interface with a token it mints from the admin CLI:
// the API is the only interface that can create a repository and commit to
// it, and the CLI is the only way to get a credential for it on an instance
// whose admin password belongs to the snapshot rather than to this drill.
//
// The run's outcome is read from the commit-status API rather than from an
// Actions-specific endpoint. Forgejo publishes every Actions job as a commit
// status on the commit that triggered it, and the combined status of a
// single-job workflow is that job's outcome — through an interface that has
// been stable across forge versions far longer than the Actions API has, on
// an instance running whatever Forgejo version the snapshot pinned
// (RSTR-002).
func smokeScript(repo, tokenName string, timeout, poll time.Duration) string {
	repoJSON := fmt.Sprintf(
		`{"name":%q,"description":"Farrier drill smoke test","private":true,"auto_init":true,"default_branch":%q}`,
		repo, smokeBranch,
	)
	workflowJSON := fmt.Sprintf(
		`{"branch":%q,"message":"farrier drill smoke job","content":%q}`,
		smokeBranch, base64.StdEncoding.EncodeToString([]byte(smokeWorkflow())),
	)

	lines := []string{
		"set -eu",
		fmt.Sprintf("api=http://localhost:%d/api/v1", HTTPPort),
		fmt.Sprintf("owner=%s", adminUsername),
		fmt.Sprintf("repo=%s", repo),
		"",
		"fail() { echo \"$1\" >&2; exit 1; }",
		"",
		// send METHOD PATH BODY DESCRIPTION: fails with the description and
		// the instance's own response when the request is not a 2xx.
		"send() {",
		"  out=$(curl -sS -X \"$1\" -H \"$auth\" -H 'Content-Type: application/json' -d \"$3\" -w '\\n%{http_code}' \"$api$2\") || fail \"$4: could not reach the instance\"",
		"  code=$(printf '%s' \"$out\" | tail -n 1)",
		"  case \"$code\" in",
		"    2*) ;;",
		"    *) fail \"$4: the instance answered HTTP $code: $(printf '%s' \"$out\" | sed '$d' | head -c 400)\" ;;",
		"  esac",
		"}",
		"",
		fmt.Sprintf(
			"token=$(forgejo admin user generate-access-token --username %s --token-name %s --scopes write:repository --raw) || "+
				"fail 'mint an api token for the smoke job: the admin account the snapshot carries could not issue one'",
			quote(adminUsername), quote(tokenName),
		),
		"auth=\"Authorization: token $token\"",
		"",
		fmt.Sprintf("send POST /user/repos %s 'create the scratch repository'", quote(repoJSON)),
		// Actions is enabled instance-wide (RenderAppINI), but whether a new
		// repository gets the unit turned on depends on the instance's
		// default units, which the snapshot's own configuration decides.
		// Turning it on explicitly makes the smoke job independent of that.
		"send PATCH \"/repos/$owner/$repo\" '{\"has_actions\":true}' 'enable actions on the scratch repository'",
		fmt.Sprintf(
			"send POST \"/repos/$owner/$repo/contents/%s\" %s 'commit the smoke workflow'",
			smokeWorkflowPath, quote(workflowJSON),
		),
		"",
		fmt.Sprintf("deadline=$(($(date +%%s) + %d))", int(timeout.Seconds())),
		"state=",
		"while :; do",
		fmt.Sprintf(
			"  status=$(curl -sS -H \"$auth\" \"$api/repos/$owner/$repo/commits/%s/status\") || fail 'read the smoke run outcome: could not reach the instance'",
			smokeBranch,
		),
		// The combined status carries the overall state first; every later
		// "state" belongs to an individual status, so the first match is the
		// one being waited on.
		"  state=$(printf '%s' \"$status\" | tr ',' '\\n' | sed -n 's/.*\"state\":\"\\([a-z]*\\)\".*/\\1/p' | head -n 1)",
		"  case \"$state\" in",
		"    success) exit 0 ;;",
		"    failure|error) fail \"the smoke run finished with state $state\" ;;",
		"  esac",
		fmt.Sprintf(
			"  [ \"$(date +%%s)\" -lt \"$deadline\" ] || fail \"the smoke run did not finish within %s (last state: ${state:-none}); "+
				"no runner claimed a job labelled %s, or it is still starting one\"",
			timeout, smokeRunsOn,
		),
		fmt.Sprintf("  sleep %d", int(poll.Seconds())),
		"done",
	}
	return strings.Join(lines, "\n")
}

// lastLine returns the final non-empty line of s. The smoke script's own
// failure messages are written last, after whatever curl or the forgejo CLI
// put on stderr before it, so this is the line that names what broke.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return s
}
