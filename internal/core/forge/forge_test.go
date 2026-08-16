package forge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// *orchestrate.Client must keep satisfying Runner, since it's the type
// Bootstrap runs against in the deployment flow.
var _ Runner = (*orchestrate.Client)(nil)

func TestNewAdminAccount(t *testing.T) {
	a, err := NewAdminAccount("forge.example.com")
	if err != nil {
		t.Fatalf("NewAdminAccount: %v", err)
	}
	if a.Username != adminUsername {
		t.Errorf("Username = %q, want %q", a.Username, adminUsername)
	}
	if want := adminUsername + "@forge.example.com"; a.Email != want {
		t.Errorf("Email = %q, want %q", a.Email, want)
	}
	if len(a.Password.Reveal()) != passwordLength {
		t.Errorf("len(Password) = %d, want %d", len(a.Password.Reveal()), passwordLength)
	}
	for _, r := range a.Password.Reveal() {
		if !strings.ContainsRune(passwordCharset, r) {
			t.Fatalf("Password contains char %q outside charset", r)
		}
	}
}

// TestAdminUsernameIsNotReserved guards the one decision behind the
// username constant. On 2026-08-10 a deployment failed with "CreateUser:
// name is reserved [name: admin]" — Forgejo reserves "admin", so `up` could
// not create the first admin account on any host, and the operator was left
// with a forge they could not log into. The obvious word is the broken one;
// this is here so the next person to tidy the constant sees that before
// they change it.
//
// Forgejo's reserved list lives upstream and will drift, so this does not
// copy it. Only a running Forgejo can answer whether a name is acceptable;
// what a unit test can do is stop a silent return to the name already known
// to fail.
func TestAdminUsernameIsNotReserved(t *testing.T) {
	if adminUsername == "admin" {
		t.Error(`adminUsername is "admin", which Forgejo reserves — CreateUser rejects it and no admin account is ever created`)
	}
	a, err := NewAdminAccount("forge.example.com")
	if err != nil {
		t.Fatalf("NewAdminAccount: %v", err)
	}
	if a.Username == "admin" {
		t.Errorf("Username = %q, which Forgejo reserves", a.Username)
	}
}

// TestSmokeRepositoryOwnerIsTheAdminAccount pins the coupling between the
// admin username and the drill smoke job's repository owner. The smoke
// script mints its API token from the admin account, so the repository it
// creates lands under that account — changing one without the other builds
// a path that points at a user who does not exist.
func TestSmokeRepositoryOwnerIsTheAdminAccount(t *testing.T) {
	a, err := NewAdminAccount("forge.example.com")
	if err != nil {
		t.Fatalf("NewAdminAccount: %v", err)
	}
	runner := &fakeRunner{}
	result, err := SmokeCI(context.Background(), runner, events.NewJob(), SmokeOptions{Repository: "smoke-repo"})
	if err != nil {
		t.Fatalf("SmokeCI: %v", err)
	}
	owner, _, ok := strings.Cut(result.Repository, "/")
	if !ok {
		t.Fatalf("Repository = %q, want owner/name form", result.Repository)
	}
	if owner != a.Username {
		t.Errorf("smoke repository owner = %q, admin username = %q — they must be the same account", owner, a.Username)
	}
	if !strings.Contains(runner.lastCmd(), "owner="+a.Username) {
		t.Errorf("smoke script does not set owner=%s: %q", a.Username, runner.lastCmd())
	}
}

func TestNewAdminAccountGeneratesFreshPasswordEachCall(t *testing.T) {
	a, err := NewAdminAccount("forge.example.com")
	if err != nil {
		t.Fatalf("NewAdminAccount: %v", err)
	}
	b, err := NewAdminAccount("forge.example.com")
	if err != nil {
		t.Fatalf("NewAdminAccount: %v", err)
	}
	if a.Password == b.Password {
		t.Fatal("two calls produced the same password")
	}
}

func TestNewAdminAccountRequiresDomain(t *testing.T) {
	if _, err := NewAdminAccount(""); err == nil {
		t.Fatal("NewAdminAccount(\"\") succeeded, want error")
	}
	if _, err := NewAdminAccount("   "); err == nil {
		t.Fatal("NewAdminAccount(whitespace) succeeded, want error")
	}
}

// fakeRunner records every command it was asked to run and plays back a
// canned result, standing in for orchestrate.Client in tests. It writes to
// both streams because the real command does: Forgejo's CLI puts some fatal
// errors on stdout.
//
// fakeRunner answers the two commands Bootstrap issues — `admin user
// create` and `admin user generate-access-token` — independently, since
// every interesting case has one of them succeeding and the other not.
// stdout/stderr/err are the create command's; the token* fields are the
// mint's.
type fakeRunner struct {
	commands []string

	stdout string
	stderr string
	err    error

	tokenStdout string
	tokenStderr string
	tokenErr    error
}

// mintMarker is the substring that tells Bootstrap's mint apart from every
// other command a test may put through this fake. It is the token's name
// rather than the CLI subcommand, because the drill's smoke script mints a
// token of its own and would otherwise match too.
const mintMarker = "--token-name '" + publishTokenName + "'"

func (f *fakeRunner) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	f.commands = append(f.commands, command)

	out, errOut, err := f.stdout, f.stderr, f.err
	if strings.Contains(command, mintMarker) {
		out, errOut, err = f.tokenStdout, f.tokenStderr, f.tokenErr
	}
	if out != "" && stdout != nil {
		stdout.Write([]byte(out))
	}
	if errOut != "" && stderr != nil {
		stderr.Write([]byte(errOut))
	}
	return err
}

// createCmd is the `admin user create` invocation the runner saw, or "".
func (f *fakeRunner) createCmd() string {
	return f.commandContaining("admin user create")
}

// mintCmd is the `generate-access-token` invocation the runner saw, or "".
func (f *fakeRunner) mintCmd() string {
	return f.commandContaining(mintMarker)
}

// lastCmd is the most recent command the runner saw, or "".
func (f *fakeRunner) lastCmd() string {
	if len(f.commands) == 0 {
		return ""
	}
	return f.commands[len(f.commands)-1]
}

func (f *fakeRunner) commandContaining(substr string) string {
	for _, c := range f.commands {
		if strings.Contains(c, substr) {
			return c
		}
	}
	return ""
}

func TestBootstrapRunsAdminUserCreate(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{}
	job := events.NewJob()

	if err := Bootstrap(context.Background(), runner, job, account); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	want := "docker compose exec -T -u git forgejo forgejo admin user create --username 'admin' --email 'admin@forge.example.com' --password 's3cret-pw' --admin --must-change-password=false"
	if runner.createCmd() != want {
		t.Errorf("command =\n%s\nwant\n%s", runner.createCmd(), want)
	}
}

// TestBootstrapRunsAsTheGitUser pins the one flag the whole deployment flow
// depends on: `docker compose exec` defaults to root, and Forgejo aborts
// outright when its CLI runs as root, so an exec without `-u git` fails on
// every host.
func TestBootstrapRunsAsTheGitUser(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{}

	if err := Bootstrap(context.Background(), runner, events.NewJob(), account); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	for _, got := range runner.commands {
		if want := "-u " + runUser + " "; !strings.Contains(got, want) {
			t.Errorf("command %q does not run as the %s user (want %q)", got, runUser, want)
		}
	}
}

// TestBootstrapReportsAFailureArrivingOnStdout is the case that made the
// root abort invisible: Forgejo's CLI wrote "Forgejo is not supposed to be
// run as root" to stdout, Bootstrap discarded stdout, and the operator got
// "command failed with no output" for a failure the container had explained
// in full.
func TestBootstrapReportsAFailureArrivingOnStdout(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	const fatal = "2026/08/10 17:34:24 ...setting.go:187:loadRunModeFrom() [F] Forgejo is not supposed to be run as root. Sorry."
	runner := &fakeRunner{stdout: fatal, err: errors.New("exit status 1")}
	job := events.NewJob()

	err := Bootstrap(context.Background(), runner, job, account)
	if err == nil {
		t.Fatal("Bootstrap succeeded, want error")
	}
	if !strings.Contains(err.Error(), "not supposed to be run as root") {
		t.Errorf("returned error %q does not carry the message the command printed", err)
	}

	last := job.Events()[len(job.Events())-1]
	if last.State != events.StateFailed {
		t.Errorf("event state = %v, want failed", last.State)
	}
	if !strings.Contains(last.Detail, "not supposed to be run as root") {
		t.Errorf("failed detail %q does not carry the message the command printed", last.Detail)
	}
	if strings.Contains(last.Detail, "no output") {
		t.Errorf("failed detail %q reports no output for a command that printed some", last.Detail)
	}
}

// TestBootstrapReportsBothStreams: a command that writes to both has both
// read, since neither stream is reliably the one carrying the explanation.
func TestBootstrapReportsBothStreams(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{stdout: "on stdout", stderr: "on stderr", err: errors.New("exit status 1")}
	job := events.NewJob()

	if err := Bootstrap(context.Background(), runner, job, account); err == nil {
		t.Fatal("Bootstrap succeeded, want error")
	}
	last := job.Events()[len(job.Events())-1]
	for _, want := range []string{"on stdout", "on stderr"} {
		if !strings.Contains(last.Detail, want) {
			t.Errorf("failed detail %q missing %q", last.Detail, want)
		}
	}
}

// TestBootstrapRedactsThePasswordFromReportedOutput: the password is on the
// command line, so a CLI that echoes its arguments back on failure would put
// it straight into the event stream. Reporting the command's own output means
// redacting it rather than reasoning about what Forgejo prints.
func TestBootstrapRedactsThePasswordFromReportedOutput(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{
		stdout: "usage: forgejo admin user create --password s3cret-pw",
		stderr: "Command error: invalid argument near --password 's3cret-pw'",
		err:    errors.New("exit status 1"),
	}
	job := events.NewJob()

	err := Bootstrap(context.Background(), runner, job, account)
	if err == nil {
		t.Fatal("Bootstrap succeeded, want error")
	}
	if strings.Contains(err.Error(), account.Password.Reveal()) {
		t.Errorf("returned error leaked the password: %v", err)
	}
	for _, ev := range job.Events() {
		if strings.Contains(ev.Detail, account.Password.Reveal()) {
			t.Errorf("event %+v leaked the password", ev)
		}
	}
	last := job.Events()[len(job.Events())-1]
	if !strings.Contains(last.Detail, redactedValue) {
		t.Errorf("failed detail %q does not mark where the password was removed", last.Detail)
	}
	if !strings.Contains(last.Detail, "invalid argument") {
		t.Errorf("failed detail %q lost the rest of the message to redaction", last.Detail)
	}
}

// TestBootstrapSkipsWhenAlreadyExistsArrivesOnStdout: UP-003's
// already-bootstrapped detection reads the same combined output as the
// failure path, so it holds wherever the CLI writes the message.
func TestBootstrapSkipsWhenAlreadyExistsArrivesOnStdout(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{stdout: "Command error: user already exists [name: admin]", err: errors.New("exit status 1")}
	job := events.NewJob()

	if err := Bootstrap(context.Background(), runner, job, account); err != nil {
		t.Fatalf("Bootstrap: %v, want nil (already-bootstrapped host is not a failure)", err)
	}
	last := job.Events()[len(job.Events())-1]
	if last.State != events.StateSucceeded {
		t.Errorf("event state = %v, want succeeded", last.State)
	}
	if strings.Contains(last.Detail, account.Password.Reveal()) {
		t.Error("skip-path event leaked a password the account doesn't have")
	}
}

func TestBootstrapEmitsCredentialsExactlyOnce(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{tokenStdout: "mintedtoken0001\n"}
	job := events.NewJob()

	if err := Bootstrap(context.Background(), runner, job, account); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	got := job.Events()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (started, succeeded): %+v", len(got), got)
	}
	if got[0].State != events.StateStarted || got[0].Step != StepAdminBootstrap {
		t.Errorf("event 0 = %+v, want step=%s state=started", got[0], StepAdminBootstrap)
	}
	if strings.Contains(got[0].Detail, account.Password.Reveal()) {
		t.Error("started event leaked the password")
	}

	last := got[len(got)-1]
	if last.State != events.StateSucceeded || last.Step != StepAdminBootstrap {
		t.Errorf("event 1 = %+v, want step=%s state=succeeded", last, StepAdminBootstrap)
	}

	occurrences := 0
	for _, ev := range got {
		occurrences += strings.Count(ev.Detail, account.Password.Reveal())
	}
	if occurrences != 1 {
		t.Errorf("password appears %d times across the event stream, want exactly 1", occurrences)
	}
	if !strings.Contains(last.Detail, account.Username) || !strings.Contains(last.Detail, account.Email) {
		t.Errorf("succeeded detail %q missing username/email", last.Detail)
	}

	if job.Done() {
		t.Error("Bootstrap ended the job; it should leave the terminal event to the caller")
	}
}

func TestBootstrapFailureEmitsFailedStepAndReturnsError(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{stderr: "database is locked", err: errors.New("exit status 1")}
	job := events.NewJob()

	err := Bootstrap(context.Background(), runner, job, account)
	if err == nil {
		t.Fatal("Bootstrap succeeded, want error")
	}

	got := job.Events()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (started, failed): %+v", len(got), got)
	}
	last := got[len(got)-1]
	if last.State != events.StateFailed {
		t.Errorf("event 1 state = %v, want failed", last.State)
	}
	if !strings.Contains(last.Detail, "database is locked") {
		t.Errorf("failed detail %q missing stderr", last.Detail)
	}
	if strings.Contains(last.Detail, account.Password.Reveal()) {
		t.Error("failed event leaked the password")
	}
	if job.Done() {
		t.Error("Bootstrap ended the job on a step failure; it should leave the terminal event to the caller")
	}
}

// TestBootstrapSkipsWhenAdminAccountAlreadyExists exercises UP-003: a
// second `up` against a host that's already been bootstrapped hits this
// exact failure from `forgejo admin user create` (Forgejo's
// ErrUserAlreadyExist, surfaced by the CLI as "Command error: user already
// exists [name: admin]" on stderr) and must treat it as done, not a
// failure — and must never emit account.Password, since the account
// doesn't actually have that password.
func TestBootstrapSkipsWhenAdminAccountAlreadyExists(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{stderr: "Command error: user already exists [name: admin]", err: errors.New("exit status 1")}
	job := events.NewJob()

	if err := Bootstrap(context.Background(), runner, job, account); err != nil {
		t.Fatalf("Bootstrap: %v, want nil (already-bootstrapped host is not a failure)", err)
	}

	got := job.Events()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (started, succeeded): %+v", len(got), got)
	}
	last := got[len(got)-1]
	if last.State != events.StateSucceeded {
		t.Errorf("event 1 state = %v, want succeeded", last.State)
	}
	if !strings.Contains(last.Detail, account.Username) {
		t.Errorf("succeeded detail %q missing username", last.Detail)
	}
	if strings.Contains(last.Detail, account.Password.Reveal()) {
		t.Error("skip-path event leaked a password the account doesn't have")
	}
	if job.Done() {
		t.Error("Bootstrap ended the job; it should leave the terminal event to the caller")
	}
}

func TestBootstrapFailureWithEmptyStderrDoesNotLeakPassword(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	// err.Error() on the real orchestrate.Client embeds the full command —
	// including the quoted password — so a fakeRunner error standing in for
	// an infra failure (dropped SSH session, canceled context) must never
	// reach the event stream or the returned error.
	runErr := fmt.Errorf("orchestrate: run %q: context canceled", fmt.Sprintf("--password %s", quote(account.Password.Reveal())))
	runner := &fakeRunner{err: runErr}
	job := events.NewJob()

	err := Bootstrap(context.Background(), runner, job, account)
	if err == nil {
		t.Fatal("Bootstrap succeeded, want error")
	}
	if strings.Contains(err.Error(), account.Password.Reveal()) {
		t.Errorf("returned error leaked the password: %v", err)
	}

	got := job.Events()
	last := got[len(got)-1]
	if last.State != events.StateFailed {
		t.Errorf("event state = %v, want failed", last.State)
	}
	if strings.Contains(last.Detail, account.Password.Reveal()) {
		t.Errorf("failed event leaked the password: %q", last.Detail)
	}
}

func TestQuoteEscapesSingleQuotes(t *testing.T) {
	got := quote(`it's`)
	want := `'it'\''s'`
	if got != want {
		t.Errorf("quote(%q) = %q, want %q", `it's`, got, want)
	}
}

func TestAdminAccountWithEmbeddedShellMetacharactersStaysQuoted(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@example.com", Password: keystore.NewSecret(`p$(whoami)'; rm -rf /`)}
	runner := &fakeRunner{}
	job := events.NewJob()

	if err := Bootstrap(context.Background(), runner, job, account); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	want := fmt.Sprintf("--password %s", quote(account.Password.Reveal()))
	if !strings.Contains(runner.createCmd(), want) {
		t.Errorf("command = %q, want it to contain %q", runner.createCmd(), want)
	}
}

// mintedToken stands in for what `admin user generate-access-token --raw`
// prints: the token on its own line, nothing else.
const mintedToken = "6f1c0aab6e9d4b2f9a3d5c7e8b0f2a4d6c8e0a1b"

// TestBootstrapMintsThePublishToken pins the whole mint invocation. It is
// the command that keeps the quick start inside the terminal, and every
// piece of it is load-bearing: `-u git` because `docker compose exec`
// defaults to root and Forgejo refuses to run as root, `--raw` because
// anything else wraps the token in a sentence, and the scopes because a
// token that cannot create a repository does not let `publish` finish.
func TestBootstrapMintsThePublishToken(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{tokenStdout: mintedToken + "\n"}

	if err := Bootstrap(context.Background(), runner, events.NewJob(), account); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	want := "docker compose exec -T -u git forgejo forgejo admin user generate-access-token " +
		"--username 'admin' --token-name 'farrier-publish' --scopes 'write:repository,write:user' --raw"
	if runner.mintCmd() != want {
		t.Errorf("mint command =\n%s\nwant\n%s", runner.mintCmd(), want)
	}
}

// TestBootstrapMintsAfterTheAccountExists: the token is minted for an
// account, so the order is not negotiable — a mint issued before the create
// lands has no user to issue against.
func TestBootstrapMintsAfterTheAccountExists(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{tokenStdout: mintedToken}

	if err := Bootstrap(context.Background(), runner, events.NewJob(), account); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(runner.commands) != 2 {
		t.Fatalf("ran %d commands, want 2 (create, mint): %q", len(runner.commands), runner.commands)
	}
	if !strings.Contains(runner.commands[0], "admin user create") {
		t.Errorf("command 0 = %q, want the create", runner.commands[0])
	}
	if !strings.Contains(runner.commands[1], mintMarker) {
		t.Errorf("command 1 = %q, want the mint", runner.commands[1])
	}
}

// TestBootstrapEmitsTheMintedToken is what the operator's terminal has to
// carry for `farrier publish` to work without a trip to the web UI: the
// token, in the same one event the credentials arrive in, and named so it
// can be found and revoked on the account later.
func TestBootstrapEmitsTheMintedToken(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{tokenStdout: mintedToken + "\n"}
	job := events.NewJob()

	if err := Bootstrap(context.Background(), runner, job, account); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	got := job.Events()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (started, succeeded): %+v", len(got), got)
	}
	last := got[len(got)-1]
	if last.State != events.StateSucceeded {
		t.Fatalf("event state = %v, want succeeded", last.State)
	}
	if !strings.Contains(last.Detail, mintedToken) {
		t.Errorf("succeeded detail %q does not carry the minted token", last.Detail)
	}
	if !strings.Contains(last.Detail, publishTokenName) {
		t.Errorf("succeeded detail %q does not name the token on the account, so the operator cannot find it to revoke", last.Detail)
	}
	if !strings.Contains(last.Detail, account.Password.Reveal()) {
		t.Errorf("succeeded detail %q dropped the password when it gained the token", last.Detail)
	}

	occurrences := 0
	for _, ev := range got {
		occurrences += strings.Count(ev.Detail, mintedToken)
	}
	if occurrences != 1 {
		t.Errorf("token appears %d times across the event stream, want exactly 1", occurrences)
	}
}

// TestPublishTokenScopesAreExactlyWhatPublishUses pins the scope string
// against the calls internal/core/publish makes. Forgejo takes the level
// from the HTTP method — anything but GET needs write, and write implies
// read — so two write scopes cover all five calls: /user and /user/keys are
// the user category, /user/repos and /repos/{owner}/{repo} the repository
// one. Anything added here is a permission an operator did not ask for.
func TestPublishTokenScopesAreExactlyWhatPublishUses(t *testing.T) {
	want := []string{"write:repository", "write:user"}
	got := strings.Split(publishTokenScopes, ",")
	if len(got) != len(want) {
		t.Fatalf("scopes = %q, want exactly %q", publishTokenScopes, want)
	}
	for i, scope := range got {
		if scope != want[i] {
			t.Errorf("scope %d = %q, want %q", i, scope, want[i])
		}
	}
	for _, unwanted := range []string{"all", "write:admin", "write:organization", "public-only"} {
		if strings.Contains(publishTokenScopes, unwanted) {
			t.Errorf("scopes %q include %q, which publish never uses", publishTokenScopes, unwanted)
		}
	}
}

// TestBootstrapMintsNothingWhenTheAccountAlreadyExists is the repeat-`up`
// case. A token per deployment would pile dead credentials onto the account
// forever, and the token this run would emit is not one the operator's
// earlier run has — so the path mints nothing, emits no credentials, and
// says where to get a new token instead.
func TestBootstrapMintsNothingWhenTheAccountAlreadyExists(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{
		stderr:      "Command error: user already exists [name: admin]",
		err:         errors.New("exit status 1"),
		tokenStdout: mintedToken,
	}
	job := events.NewJob()

	if err := Bootstrap(context.Background(), runner, job, account); err != nil {
		t.Fatalf("Bootstrap: %v, want nil (already-bootstrapped host is not a failure)", err)
	}

	if runner.mintCmd() != "" {
		t.Errorf("already-exists path minted a token: %q", runner.mintCmd())
	}
	if len(runner.commands) != 1 {
		t.Errorf("ran %d commands, want 1 (the create attempt alone): %q", len(runner.commands), runner.commands)
	}
	for _, ev := range job.Events() {
		if strings.Contains(ev.Detail, mintedToken) {
			t.Errorf("already-exists event %+v emitted a token the account does not have", ev)
		}
	}
	last := job.Events()[len(job.Events())-1]
	if !strings.Contains(last.Detail, "web UI") {
		t.Errorf("already-exists detail %q does not tell the operator where to create a new token", last.Detail)
	}
}

// TestBootstrapReportsAFailingMintFromBothStreams: the mint is a Forgejo
// CLI command through `docker compose exec`, so it fails the same two ways
// `admin user create` does — its own error on stderr, the run-as-root abort
// on stdout — and the operator gets whichever one carries the message.
func TestBootstrapReportsAFailingMintFromBothStreams(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{
		tokenStdout: "Forgejo is not supposed to be run as root. Sorry.",
		tokenStderr: "Command error: access token name has been used already",
		tokenErr:    errors.New("exit status 1"),
	}
	job := events.NewJob()

	if err := Bootstrap(context.Background(), runner, job, account); err != nil {
		t.Fatalf("Bootstrap: %v, want nil (the account exists; only its token is missing)", err)
	}

	last := job.Events()[len(job.Events())-1]
	for _, want := range []string{"not supposed to be run as root", "access token name has been used already"} {
		if !strings.Contains(last.Detail, want) {
			t.Errorf("detail %q missing %q", last.Detail, want)
		}
	}
	if !strings.Contains(last.Detail, "web UI") {
		t.Errorf("detail %q does not tell the operator where to create the token by hand", last.Detail)
	}
}

// TestBootstrapStillEmitsCredentialsWhenTheMintFails is why a failing mint
// is not a failing step. The password in account was generated for this
// call and exists nowhere else; a second `up` takes the already-exists path
// and never emits it. Losing the token costs a visit to the web UI, losing
// the password costs the account — so the credentials go out either way.
func TestBootstrapStillEmitsCredentialsWhenTheMintFails(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{tokenStderr: "database is locked", tokenErr: errors.New("exit status 1")}
	job := events.NewJob()

	if err := Bootstrap(context.Background(), runner, job, account); err != nil {
		t.Fatalf("Bootstrap: %v, want nil", err)
	}

	got := job.Events()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (started, succeeded): %+v", len(got), got)
	}
	last := got[len(got)-1]
	if last.State != events.StateSucceeded {
		t.Errorf("event state = %v, want succeeded — the account was created", last.State)
	}
	if !strings.Contains(last.Detail, account.Password.Reveal()) {
		t.Fatalf("detail %q dropped the password on a failing mint, stranding the operator", last.Detail)
	}
	if !strings.Contains(last.Detail, "database is locked") {
		t.Errorf("detail %q does not say why the token is missing", last.Detail)
	}
}

// TestBootstrapDoesNotLeakTheTokenOnAFailingMint mirrors the password's own
// redaction test for the token: a command that prints the token and then
// exits non-zero must not have it reported back. The token is read out of
// stdout before anything is reported, precisely so it can be removed from
// what is.
func TestBootstrapDoesNotLeakTheTokenOnAFailingMint(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{
		tokenStdout: mintedToken,
		tokenStderr: "Command error: writing token " + mintedToken + " to the database failed",
		tokenErr:    errors.New("exit status 1"),
	}
	job := events.NewJob()

	if err := Bootstrap(context.Background(), runner, job, account); err != nil {
		t.Fatalf("Bootstrap: %v, want nil", err)
	}

	for _, ev := range job.Events() {
		if strings.Contains(ev.Detail, mintedToken) {
			t.Errorf("event %+v leaked a token that was never issued", ev)
		}
	}
	last := job.Events()[len(job.Events())-1]
	if !strings.Contains(last.Detail, redactedValue) {
		t.Errorf("detail %q does not mark where the token was removed", last.Detail)
	}
	if !strings.Contains(last.Detail, "to the database failed") {
		t.Errorf("detail %q lost the rest of the message to redaction", last.Detail)
	}
}

// TestBootstrapMintFailureWithNoOutputDoesNotLeak: the transport's error
// text embeds the whole command it ran, so the mint path may no more use
// err.Error() than the create path may. Nothing but the command's own
// output — redacted — reaches the operator.
func TestBootstrapMintFailureWithNoOutputDoesNotLeak(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{tokenErr: fmt.Errorf("orchestrate: run %q: context canceled", tokenCommand(account))}
	job := events.NewJob()

	if err := Bootstrap(context.Background(), runner, job, account); err != nil {
		t.Fatalf("Bootstrap: %v, want nil", err)
	}
	last := job.Events()[len(job.Events())-1]
	if strings.Contains(last.Detail, "context canceled") {
		t.Errorf("detail %q reports the transport's error text, which embeds the command", last.Detail)
	}
	if !strings.Contains(last.Detail, "no output") {
		t.Errorf("detail %q does not say the command explained nothing", last.Detail)
	}
}

// TestBootstrapTreatsASilentMintAsNoToken: `--raw` prints the token and
// nothing else, so an exit-zero mint with an empty stdout has produced no
// token to hand over. Emitting the empty string as one would send the
// operator off to debug a token that does not exist.
func TestBootstrapTreatsASilentMintAsNoToken(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	job := events.NewJob()

	if err := Bootstrap(context.Background(), &fakeRunner{}, job, account); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	last := job.Events()[len(job.Events())-1]
	if !strings.Contains(last.Detail, "no token") {
		t.Errorf("detail %q does not say the command produced no token", last.Detail)
	}
	if !strings.Contains(last.Detail, "web UI") {
		t.Errorf("detail %q does not tell the operator where to create one", last.Detail)
	}
}

// TestRedactRemovesEveryValue: redact grew a second secret when the token
// arrived, and a loop that stops after the first one leaks the rest.
func TestRedactRemovesEveryValue(t *testing.T) {
	got := redact("password pw and token tk and pw again", "pw", "tk")
	want := "password " + redactedValue + " and token " + redactedValue + " and " + redactedValue + " again"
	if got != want {
		t.Errorf("redact = %q, want %q", got, want)
	}
	if got := redact("nothing to remove", "", ""); got != "nothing to remove" {
		t.Errorf("redact with empty secrets = %q, want the input unchanged", got)
	}
}
