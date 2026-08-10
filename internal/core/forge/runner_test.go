package forge

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
)

func TestNewRunnerSecretHasForgejosFormat(t *testing.T) {
	secret, err := NewRunnerSecret()
	if err != nil {
		t.Fatalf("NewRunnerSecret: %v", err)
	}
	if len(secret) != 40 {
		t.Errorf("len(secret) = %d, want 40", len(secret))
	}
	if _, err := hex.DecodeString(secret); err != nil {
		t.Errorf("secret %q is not hexadecimal: %v", secret, err)
	}
	if strings.ToLower(secret) != secret {
		t.Errorf("secret %q is not lowercase", secret)
	}
	if err := ValidateRunnerSecret(secret); err != nil {
		t.Errorf("ValidateRunnerSecret on a generated secret: %v", err)
	}
}

func TestNewRunnerSecretGeneratesFreshSecretEachCall(t *testing.T) {
	a, err := NewRunnerSecret()
	if err != nil {
		t.Fatalf("NewRunnerSecret: %v", err)
	}
	b, err := NewRunnerSecret()
	if err != nil {
		t.Fatalf("NewRunnerSecret: %v", err)
	}
	if a == b {
		t.Fatal("two calls produced the same secret")
	}
}

func TestValidateRunnerSecretRejectsMalformedSecrets(t *testing.T) {
	cases := map[string]string{
		"empty":     "",
		"too short": strings.Repeat("a", 39),
		"too long":  strings.Repeat("a", 41),
		"not hex":   strings.Repeat("z", 40),
		"uppercase": strings.Repeat("A", 40),
	}
	for name, secret := range cases {
		if err := ValidateRunnerSecret(secret); err == nil {
			t.Errorf("ValidateRunnerSecret(%s) succeeded, want error", name)
		}
	}
}

func TestInstanceURLIsTheBundleDomainOverHTTPS(t *testing.T) {
	m := &bundle.Manifest{Domain: " forge.example.com "}
	if got, want := InstanceURL(m, ""), "https://forge.example.com/"; got != want {
		t.Errorf("InstanceURL = %q, want %q", got, want)
	}
}

// UP-006: a nameless bundle is reached at the operator-supplied address
// over plain HTTP, and the runner is pointed at the same URL the operator's
// browser opens.
func TestInstanceURLIsTheSuppliedAddressOverHTTPForANamelessBundle(t *testing.T) {
	m := &bundle.Manifest{}
	if got, want := InstanceURL(m, " box.tail1234.ts.net "), "http://box.tail1234.ts.net:8222/"; got != want {
		t.Errorf("InstanceURL = %q, want %q", got, want)
	}
	if got, want := InstanceURL(m, "192.168.1.5"), "http://192.168.1.5:8222/"; got != want {
		t.Errorf("InstanceURL = %q, want %q", got, want)
	}
}

func TestRunnerCommandDerivesCredentialsFromTheMountedSecret(t *testing.T) {
	cmd := RunnerCommand("https://forge.example.com/")

	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-ec" {
		t.Fatalf("command = %v, want a `sh -ec <script>` list", cmd)
	}
	script := cmd[2]

	for _, want := range []string{
		"cd '" + RunnerDataDir + "'",
		"if [ ! -f .runner ]",
		"forgejo-runner create-runner-file --instance 'https://forge.example.com/'",
		"$(cat '" + RunnerSecretFilename + "')",
		"exec forgejo-runner daemon",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
}

// The secret is key material (KEY-003): it may live in the mounted file and
// nowhere else, so it must never be interpolated into the Compose command,
// where `docker inspect` and the rendered Compose file would both carry it.
func TestRunnerCommandCarriesNoSecretValue(t *testing.T) {
	secret, err := NewRunnerSecret()
	if err != nil {
		t.Fatalf("NewRunnerSecret: %v", err)
	}
	for _, arg := range RunnerCommand("https://forge.example.com/") {
		if strings.Contains(arg, secret) {
			t.Fatalf("command argument %q carries the runner secret", arg)
		}
	}
}

func TestRegisterRunnerRunsForgejoOfflineRegistration(t *testing.T) {
	runner := &fakeRunner{}
	job := events.NewJob()

	if err := RegisterRunner(context.Background(), runner, job, "/opt/farrier/runner/secret"); err != nil {
		t.Fatalf("RegisterRunner: %v", err)
	}

	want := "docker compose exec -T -u git forgejo forgejo forgejo-cli actions register --secret-stdin --name 'farrier-colocated' < '/opt/farrier/runner/secret'"
	if runner.lastCmd() != want {
		t.Errorf("command =\n%s\nwant\n%s", runner.lastCmd(), want)
	}
}

// The secret reaches the CLI on stdin, redirected from the host file, and
// is never an argument — so it cannot land in the host's process list or in
// transport error text quoting the command (KEY-003).
func TestRegisterRunnerFeedsTheSecretOnStdinNotAsAnArgument(t *testing.T) {
	runner := &fakeRunner{}
	job := events.NewJob()

	if err := RegisterRunner(context.Background(), runner, job, "/opt/farrier/runner/secret"); err != nil {
		t.Fatalf("RegisterRunner: %v", err)
	}

	if !strings.Contains(runner.lastCmd(), "--secret-stdin") {
		t.Errorf("command does not read the secret from stdin: %s", runner.lastCmd())
	}
	if strings.Contains(runner.lastCmd(), "|") {
		t.Errorf("command pipes into docker compose, which would drop the Compose project environment: %s", runner.lastCmd())
	}
	if !strings.HasSuffix(runner.lastCmd(), "< '/opt/farrier/runner/secret'") {
		t.Errorf("command does not redirect the secret file into stdin: %s", runner.lastCmd())
	}
}

func TestRegisterRunnerEmitsAStepAndLeavesTheJobOpen(t *testing.T) {
	runner := &fakeRunner{}
	job := events.NewJob()

	if err := RegisterRunner(context.Background(), runner, job, "/opt/farrier/runner/secret"); err != nil {
		t.Fatalf("RegisterRunner: %v", err)
	}

	got := job.Events()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (started, succeeded): %+v", len(got), got)
	}
	if got[0].Step != StepRunnerRegister || got[0].State != events.StateStarted {
		t.Errorf("event 0 = %+v, want step=%s state=started", got[0], StepRunnerRegister)
	}
	if got[1].Step != StepRunnerRegister || got[1].State != events.StateSucceeded {
		t.Errorf("event 1 = %+v, want step=%s state=succeeded", got[1], StepRunnerRegister)
	}
	if job.Done() {
		t.Error("RegisterRunner ended the job; it should leave the terminal event to the caller")
	}
}

// Re-running `up` must not fail because the registration is already there
// (UP-003): registration by secret is an upsert, and a Forgejo that reports
// the existing registration as an error is still a converged host.
func TestRegisterRunnerTreatsAnExistingRegistrationAsDone(t *testing.T) {
	runner := &fakeRunner{stderr: "runner already exists", err: errors.New("exit status 1")}
	job := events.NewJob()

	if err := RegisterRunner(context.Background(), runner, job, "/opt/farrier/runner/secret"); err != nil {
		t.Fatalf("RegisterRunner on an already-registered instance: %v", err)
	}

	last := job.Events()[len(job.Events())-1]
	if last.State != events.StateSucceeded {
		t.Errorf("last event = %+v, want succeeded", last)
	}
}

func TestRegisterRunnerFailureEmitsFailedStepAndReturnsError(t *testing.T) {
	runner := &fakeRunner{stderr: "database is locked", err: errors.New("exit status 1")}
	job := events.NewJob()

	err := RegisterRunner(context.Background(), runner, job, "/opt/farrier/runner/secret")
	if err == nil {
		t.Fatal("RegisterRunner succeeded, want error")
	}
	if !strings.Contains(err.Error(), "database is locked") {
		t.Errorf("error %q does not carry the command's stderr", err)
	}

	last := job.Events()[len(job.Events())-1]
	if last.State != events.StateFailed || last.Step != StepRunnerRegister {
		t.Errorf("last event = %+v, want step=%s state=failed", last, StepRunnerRegister)
	}
}

// A registration that fails with its explanation on stdout is reported the
// same as one that fails on stderr — being blind on one stream is what made
// the admin-bootstrap abort look like a command with no output at all.
func TestRegisterRunnerReportsAFailureArrivingOnStdout(t *testing.T) {
	runner := &fakeRunner{stdout: "Forgejo is not supposed to be run as root. Sorry.", err: errors.New("exit status 1")}
	job := events.NewJob()

	err := RegisterRunner(context.Background(), runner, job, "/opt/farrier/runner/secret")
	if err == nil {
		t.Fatal("RegisterRunner succeeded, want error")
	}
	if !strings.Contains(err.Error(), "not supposed to be run as root") {
		t.Errorf("error %q does not carry what the command printed", err)
	}

	last := job.Events()[len(job.Events())-1]
	if !strings.Contains(last.Detail, "not supposed to be run as root") {
		t.Errorf("event detail %q does not carry what the command printed", last.Detail)
	}
}

func TestRegisterRunnerRequiresASecretPath(t *testing.T) {
	runner := &fakeRunner{}
	job := events.NewJob()

	if err := RegisterRunner(context.Background(), runner, job, "  "); err == nil {
		t.Fatal("RegisterRunner with no secret path succeeded, want error")
	}
	if runner.lastCmd() != "" {
		t.Errorf("ran a command anyway: %s", runner.lastCmd())
	}
}
