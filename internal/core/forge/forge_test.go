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
	if a.Username != "admin" {
		t.Errorf("Username = %q, want %q", a.Username, "admin")
	}
	if a.Email != "admin@forge.example.com" {
		t.Errorf("Email = %q, want %q", a.Email, "admin@forge.example.com")
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

// fakeRunner records the command it was asked to run and plays back a
// canned result, standing in for orchestrate.Client in tests. It writes to
// both streams because the real command does: Forgejo's CLI puts some fatal
// errors on stdout.
type fakeRunner struct {
	gotCommand string
	stdout     string
	stderr     string
	err        error
}

func (f *fakeRunner) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	f.gotCommand = command
	if f.stdout != "" && stdout != nil {
		stdout.Write([]byte(f.stdout))
	}
	if f.stderr != "" && stderr != nil {
		stderr.Write([]byte(f.stderr))
	}
	return f.err
}

func TestBootstrapRunsAdminUserCreate(t *testing.T) {
	account := AdminAccount{Username: "admin", Email: "admin@forge.example.com", Password: keystore.NewSecret("s3cret-pw")}
	runner := &fakeRunner{}
	job := events.NewJob()

	if err := Bootstrap(context.Background(), runner, job, account); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	want := "docker compose exec -T -u git forgejo forgejo admin user create --username 'admin' --email 'admin@forge.example.com' --password 's3cret-pw' --admin --must-change-password=false"
	if runner.gotCommand != want {
		t.Errorf("command =\n%s\nwant\n%s", runner.gotCommand, want)
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
	if want := "-u " + runUser + " "; !strings.Contains(runner.gotCommand, want) {
		t.Errorf("command %q does not run as the %s user (want %q)", runner.gotCommand, runUser, want)
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
	if !strings.Contains(last.Detail, redactedPassword) {
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
	runner := &fakeRunner{}
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
	if !strings.Contains(runner.gotCommand, want) {
		t.Errorf("command = %q, want it to contain %q", runner.gotCommand, want)
	}
}
