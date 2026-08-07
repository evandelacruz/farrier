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
// canned result, standing in for orchestrate.Client in tests.
type fakeRunner struct {
	gotCommand string
	stderr     string
	err        error
}

func (f *fakeRunner) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	f.gotCommand = command
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

	want := "docker compose exec -T forgejo forgejo admin user create --username 'admin' --email 'admin@forge.example.com' --password 's3cret-pw' --admin --must-change-password=false"
	if runner.gotCommand != want {
		t.Errorf("command =\n%s\nwant\n%s", runner.gotCommand, want)
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
	runner := &fakeRunner{stderr: "user already exists", err: errors.New("exit status 1")}
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
	if !strings.Contains(last.Detail, "user already exists") {
		t.Errorf("failed detail %q missing stderr", last.Detail)
	}
	if strings.Contains(last.Detail, account.Password.Reveal()) {
		t.Error("failed event leaked the password")
	}
	if job.Done() {
		t.Error("Bootstrap ended the job on a step failure; it should leave the terminal event to the caller")
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
