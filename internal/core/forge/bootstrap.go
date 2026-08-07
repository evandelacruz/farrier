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
// ...]", surfaced by the CLI as "Command error: <that>" on stderr. Bootstrap
// matches on it to tell "this host is already bootstrapped" (UP-003: safe
// to repeat) apart from a genuine failure.
const alreadyExistsMarker = "already exists"

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

	var stderr bytes.Buffer
	if err := runner.Run(ctx, createCommand(account), io.Discard, &stderr); err != nil {
		// err.Error() is never used here: orchestrate.Client embeds the full
		// command — including the quoted password — in its error text, and
		// that would leak the password into the event stream and caller
		// logs on infra failures (empty stderr) such as a dropped SSH
		// session or a canceled context.
		detail := "command failed with no output"
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			detail = msg
		}
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
func createCommand(a AdminAccount) string {
	return fmt.Sprintf(
		"docker compose exec -T %s forgejo admin user create --username %s --email %s --password %s --admin --must-change-password=false",
		Service, quote(a.Username), quote(a.Email), quote(a.Password.Reveal()),
	)
}

// quote wraps s in single quotes for a POSIX shell, escaping any single
// quote it contains.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
