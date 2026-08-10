package forge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/events"
)

// StepAdminBootstrap identifies the admin-bootstrap step in a deployment
// job's event stream.
const StepAdminBootstrap = "admin-bootstrap"

// Runner executes a command on the forge host, streaming its output. It is
// satisfied by *orchestrate.Client; kept as an interface here so forge
// stays decoupled from the SSH transport and testable without one.
type Runner interface {
	Run(ctx context.Context, command string, stdout, stderr io.Writer) error
}

// alreadyExistsMarker is the substring Forgejo's `admin user create` fails
// with when account.Username is already taken — the shape of
// user_model.ErrUserAlreadyExist's Error(), "user already exists [name:
// ...]", surfaced by the CLI as "Command error: <that>". Bootstrap matches
// on it across both output streams, since which one the CLI writes a given
// failure to is not something it commits to, to tell "this host is already
// bootstrapped" (UP-003: safe to repeat) apart from a genuine failure.
const alreadyExistsMarker = "user already exists"

// redactedValue is what key material is replaced with in anything reported
// to the operator — the admin password here, the app.ini secrets in
// Secrets.Redact.
const redactedValue = "[redacted]"

// FailureDetail is what a failed command left for the operator to read.
//
// Both streams are captured because Forgejo's CLI does not commit to one:
// its refusal to run as root — the failure a missing `-u git` produces —
// goes to stdout, while `admin user create`'s own errors go to stderr, and a
// caller reading only one of them reports a failure with no message at all.
// stderr comes first when both carry something, so the stream a command that
// distinguishes them puts its error on leads.
//
// Exported because every caller that runs a Forgejo command through a
// Runner needs it, not only the ones in this package: internal/core/deploy's
// readiness probes (ReadyCommand) are Forgejo CLI invocations and fail the
// same silent way without it.
func FailureDetail(stdout, stderr *bytes.Buffer) string {
	parts := make([]string, 0, 2)
	for _, buf := range []*bytes.Buffer{stderr, stdout} {
		if msg := strings.TrimSpace(buf.String()); msg != "" {
			parts = append(parts, msg)
		}
	}
	if len(parts) == 0 {
		return "command failed with no output"
	}
	return strings.Join(parts, "\n")
}

// redact replaces every occurrence of secret in s. Reporting a command's own
// output means reporting whatever the command chose to echo, and the admin
// password is on that command line — cheaper to be certain than to reason
// about what Forgejo prints back.
func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, redactedValue)
}

// Bootstrap creates account on the running forgejo service by running
// `forgejo admin user create` through runner, then emits the account's
// credentials exactly once through job's event stream (FORGE-002) — the
// only place they are ever handed to the operator. Bootstrap does not emit
// a job-terminal event; the caller's deployment flow owns that.
//
// If account.Username already exists — the host was already bootstrapped
// by an earlier `up` — Bootstrap treats that as success, not failure
// (UP-003): the account was provisioned and its credentials already
// emitted, once, on that earlier run; account.Password is a fresh secret
// generated for this call and is never used or emitted in this case, since
// re-emitting it would hand the operator a password the account doesn't
// actually have.
func Bootstrap(ctx context.Context, runner Runner, job *events.Job, account AdminAccount) error {
	job.Started(StepAdminBootstrap, "creating first admin account")

	var stdout, stderr bytes.Buffer
	if err := runner.Run(ctx, createCommand(account), &stdout, &stderr); err != nil {
		// err.Error() is never used here: orchestrate.Client embeds the full
		// command — including the quoted password — in its error text, and
		// that would leak the password into the event stream and caller
		// logs on infra failures (no output at all) such as a dropped SSH
		// session or a canceled context. That rules out the transport's
		// error, not the command's own output, which is captured from both
		// streams and redacted before it goes anywhere.
		detail := redact(FailureDetail(&stdout, &stderr), account.Password.Reveal())
		if strings.Contains(detail, alreadyExistsMarker) {
			job.Emit(StepAdminBootstrap, events.StateSucceeded, fmt.Sprintf(
				"admin account %s already exists, leaving it as-is", account.Username,
			))
			return nil
		}
		job.Emit(StepAdminBootstrap, events.StateFailed, fmt.Sprintf("create admin account: %s", detail))
		return fmt.Errorf("forge: create admin account: %s", detail)
	}

	job.Emit(StepAdminBootstrap, events.StateSucceeded, fmt.Sprintf(
		"first admin account created — username: %s, email: %s, password: %s",
		account.Username, account.Email, account.Password.Reveal(),
	))
	return nil
}

// createCommand builds the `docker compose exec` invocation that creates
// account inside the forgejo service. Every argument comes from
// AdminAccount, whose fields are either a fixed constant or generated from
// a shell-safe charset (NewAdminAccount) — quoted here regardless, so the
// command stays safe even if a caller builds an AdminAccount by hand.
//
// It runs as the git user, the form Forgejo documents for its admin CLI
// under Docker, so the command touches the database as the user that owns
// it — and so it runs at all: `docker compose exec` defaults to root, and
// Forgejo refuses to run as root outright. The container's own entrypoint
// drops to this user, so the server is unaffected either way; only an exec
// into it has to say so.
func createCommand(a AdminAccount) string {
	return fmt.Sprintf(
		"docker compose exec -T -u %s %s forgejo admin user create --username %s --email %s --password %s --admin --must-change-password=false",
		runUser, Service, quote(a.Username), quote(a.Email), quote(a.Password.Reveal()),
	)
}

// ReadyCommand is the command that answers whether Forgejo is ready to be
// used, as opposed to merely running: it succeeds once Forgejo can open its
// database and query the user table.
//
// That distinction is the whole point of it. On a host whose state
// directory is fresh, Forgejo's first boot runs its entire migration set to
// create the schema, and it accepts an exec into its container seconds
// before that finishes — so a caller that only checks the container goes
// straight to Bootstrap and meets a database with no tables in it. `admin
// user list` walks the same initDB-then-query-the-user-table path `admin
// user create` needs, has no side effects, and needs nothing in the image
// beyond Forgejo's own binary.
//
// It runs as the git user for the reason createCommand does: `docker
// compose exec` defaults to root and Forgejo refuses to run as root, so a
// probe without `-u git` fails forever on something that has nothing to do
// with readiness.
func ReadyCommand() string {
	return fmt.Sprintf("docker compose exec -T -u %s %s forgejo admin user list", runUser, Service)
}

// quote wraps s in single quotes for a POSIX shell, escaping any single
// quote it contains.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
