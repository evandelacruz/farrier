package forge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
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
// to the operator — the admin password and its publish token here, the
// app.ini secrets in Secrets.Redact.
const redactedValue = "[redacted]"

// publishTokenName is the name the token Bootstrap mints carries on the
// admin account. It is fixed and identifying on purpose: an operator
// looking at the account's token list in the forge's web UI can tell which
// token Farrier issued and revoke exactly that one.
//
// Fixed also means it can only ever be minted once per account, since
// Forgejo refuses a second token with a name the account already has
// ("access token name has been used already"). That is the behavior this
// package wants — see Bootstrap on why a repeat `up` mints nothing — but it
// is upstream's rule, not something enforced here, so nothing may mint under
// this name on a path that can run twice.
const publishTokenName = "farrier-publish"

// publishTokenScopes is the exact scope set `farrier publish` needs, and
// nothing beyond it. Derived from the calls internal/core/publish makes,
// against how Forgejo's API decides a route's required scope: a route
// declares a scope *category*, and the level within it comes from the HTTP
// method — anything but GET requires write, and write implies read.
//
//   - GET /api/v1/user, GET+POST /api/v1/user/keys — the user category,
//     write for the POST that registers a push key: write:user
//   - POST /api/v1/user/repos, GET+DELETE /api/v1/repos/{owner}/{repo} —
//     the repository category, write for the create and the cleanup
//     delete: write:repository
//
// Deliberately absent: write:organization, which publishing under an
// organization owner (POST /api/v1/orgs/{org}/repos) would need. A freshly
// deployed instance has no organizations, and a token minted at `up` that
// could create them would be broader than the quick start it exists for.
// An operator publishing into an organization creates their own token.
const publishTokenScopes = "write:repository,write:user"

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

// redact replaces every occurrence of each secret in s. Reporting a
// command's own output means reporting whatever the command chose to echo,
// and the admin password is on that command line — cheaper to be certain
// than to reason about what Forgejo prints back. The minted publish token is
// never on a command line, but it is the whole of one command's stdout, so
// it goes through here for the same reason.
func redact(s string, secrets ...string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		s = strings.ReplaceAll(s, secret, redactedValue)
	}
	return s
}

// Bootstrap creates account on the running forgejo service by running
// `forgejo admin user create` through runner, mints the access token
// publishing a project needs (IMPT-004), and emits both through job's event
// stream exactly once (FORGE-002) — the only place they are ever handed to
// the operator. Bootstrap does not emit a job-terminal event; the caller's
// deployment flow owns that.
//
// Minting the token here is what keeps the operator in the terminal: the
// alternative is sending them to the forge's web UI between `up` and
// `publish` to create one by hand, which is the one step of the quick start
// that leaves the shell and the one that costs an operator a failed run.
//
// If account.Username already exists — the host was already bootstrapped
// by an earlier `up` — Bootstrap treats that as success, not failure
// (UP-003): the account was provisioned and its credentials already
// emitted, once, on that earlier run; account.Password is a fresh secret
// generated for this call and is never used or emitted in this case, since
// re-emitting it would hand the operator a password the account doesn't
// actually have. Nothing is minted on that path either, for a related but
// distinct reason: a token minted per `up` would accumulate on the account
// forever, one dead credential per deployment. The event says so instead,
// and points at the web UI for an operator who lost theirs.
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
				"admin account %s already exists, leaving it as-is — its password and access token were "+
					"issued once, on the deployment that created it, and are not issued again here; "+
					"if the token was lost, sign in as that account and create a new one under "+
					"Settings, Applications in the forge's web UI",
				account.Username,
			))
			return nil
		}
		job.Emit(StepAdminBootstrap, events.StateFailed, fmt.Sprintf("create admin account: %s", detail))
		return fmt.Errorf("forge: create admin account: %s", detail)
	}

	token, mintFailure := mintPublishToken(ctx, runner, account)
	job.Emit(StepAdminBootstrap, events.StateSucceeded, credentialsDetail(account, token, mintFailure))
	return nil
}

// credentialsDetail is the one message the account's credentials are ever
// handed to the operator in: username, email, password, and the access
// token `farrier publish` reads from FARRIER_TARGET_TOKEN.
//
// mintFailure non-empty means the account exists but the token does not, and
// the operator is told what went wrong and where to create one by hand. The
// credentials still go out in that case, and the step still succeeds,
// because the account is real: failing here would strand the operator with
// an admin account whose password was generated for this call, emitted
// nowhere, and never re-emitted — a repeat `up` takes the already-exists
// path above. A missing token costs a visit to the web UI; a missing
// password costs the account.
func credentialsDetail(account AdminAccount, token keystore.Secret, mintFailure string) string {
	detail := fmt.Sprintf(
		"first admin account created — username: %s, email: %s, password: %s",
		account.Username, account.Email, account.Password.Reveal(),
	)
	if mintFailure != "" {
		return detail + fmt.Sprintf(
			" — an access token for publishing could not be created (%s); create one under Settings, "+
				"Applications in the forge's web UI, with the repository and user permissions set to write",
			mintFailure,
		)
	}
	return detail + fmt.Sprintf(
		", access token for publishing (named %s on the account, revoke it there when you are done with it): %s",
		publishTokenName, token.Reveal(),
	)
}

// mintPublishToken creates the access token `farrier publish` authenticates
// with, scoped to publishTokenScopes, and returns it. A non-empty second
// return is the reason there is no token, already redacted and ready to
// report; the two are never both meaningful.
//
// The token is parsed out of stdout before anything is reported, not after,
// so a command that prints the token and *then* fails still has it redacted
// out of what the operator sees. Both streams are read and the transport's
// own error is never used, for the reasons Bootstrap and FailureDetail give.
func mintPublishToken(ctx context.Context, runner Runner, account AdminAccount) (keystore.Secret, string) {
	var stdout, stderr bytes.Buffer
	err := runner.Run(ctx, tokenCommand(account), &stdout, &stderr)
	token := tokenFrom(stdout.String())
	if err != nil {
		return keystore.Secret{}, redact(FailureDetail(&stdout, &stderr), account.Password.Reveal(), token)
	}
	if token == "" {
		return keystore.Secret{}, "the command printed no token"
	}
	return keystore.NewSecret(token), ""
}

// tokenFrom reads the token out of the mint command's stdout, or returns ""
// when stdout plainly holds something else.
//
// `--raw` makes the token the whole of stdout, so the last non-empty line is
// it; the shape check is there for the failure path, which runs this over
// output that is a message rather than a token. Redacting the wrong thing
// there would delete the explanation the operator needs — the run-as-root
// abort reduced to "[redacted]" is the exact failure this package already
// spent a release fixing. A token is one whitespace-free word and Forgejo's
// are forty characters; prose is neither.
func tokenFrom(stdout string) string {
	const minTokenLength = 16
	line := lastLine(stdout)
	if len(strings.Fields(line)) != 1 || len(line) < minTokenLength {
		return ""
	}
	return line
}

// tokenCommand builds the `docker compose exec` invocation that mints the
// publish token for account. It runs as the git user for the reason
// createCommand does — `docker compose exec` defaults to root and Forgejo
// refuses to run as root — and `--raw` is what makes stdout the token alone
// rather than a sentence with the token inside it.
func tokenCommand(a AdminAccount) string {
	return fmt.Sprintf(
		"docker compose exec -T -u %s %s forgejo admin user generate-access-token --username %s --token-name %s --scopes %s --raw",
		runUser, Service, quote(a.Username), quote(publishTokenName), quote(publishTokenScopes),
	)
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
