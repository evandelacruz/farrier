package forge

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

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
		"forgejo-runner create-runner-file -c '" + RunnerConfigFilename + "' --instance 'https://forge.example.com/'",
		"$(cat '" + RunnerSecretFilename + "')",
		"exec forgejo-runner daemon -c '" + RunnerConfigFilename + "'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
}

// testJobImage and testToolCache stand in for the manifest's job image and
// the host tool cache path in this file's rendering tests.
const (
	testJobImage  = "docker.io/library/node:22-bookworm"
	testToolCache = "/opt/farrier/runner/toolcache"
)

// decodedRunnerConfig is the shape of the file Farrier writes, decoded back
// with the same keys forgejo-runner reads it under.
type decodedRunnerConfig struct {
	Runner struct {
		Labels []string `yaml:"labels"`
	} `yaml:"runner"`
	Container *struct {
		Options      string   `yaml:"options"`
		ValidVolumes []string `yaml:"valid_volumes"`
	} `yaml:"container"`
}

func decodeRunnerConfig(t *testing.T, raw []byte) decodedRunnerConfig {
	t.Helper()
	var got decodedRunnerConfig
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatalf("rendered config is not YAML: %v\n%s", err, raw)
	}
	return got
}

func TestRenderRunnerConfigDeclaresDockerAndUbuntuLatest(t *testing.T) {
	got := decodeRunnerConfig(t, RenderRunnerConfig(testJobImage, testToolCache))

	want := []string{
		"docker:docker://" + testJobImage,
		"ubuntu-latest:docker://" + testJobImage,
	}
	if len(got.Runner.Labels) != len(RunnerLabelNames) {
		t.Fatalf("config declares %d labels, want %d: %v", len(got.Runner.Labels), len(RunnerLabelNames), got.Runner.Labels)
	}
	for i, label := range want {
		if got.Runner.Labels[i] != label {
			t.Errorf("label %d = %q, want %q", i, got.Runner.Labels[i], label)
		}
	}
}

// The job image is the manifest's, not a constant this package holds: an
// operator who points the bundle at a fatter image gets it behind every
// label, not just some.
func TestRenderRunnerConfigUsesTheSuppliedJobImage(t *testing.T) {
	const image = "ghcr.io/catthehacker/ubuntu:act-22.04"
	got := decodeRunnerConfig(t, RenderRunnerConfig(image, testToolCache))

	for _, label := range got.Runner.Labels {
		if !strings.HasSuffix(label, ":docker://"+image) {
			t.Errorf("label %q does not point at the supplied image %q", label, image)
		}
	}
}

// The mount source is the path as the *host* sees it. The runner starts job
// containers on the host's Docker daemon, so the daemon resolves this in the
// host filesystem; the runner's own view of the same directory, under
// RunnerDataDir, would mount something else entirely or nothing at all.
func TestRenderRunnerConfigMountsTheToolCacheByHostPath(t *testing.T) {
	got := decodeRunnerConfig(t, RenderRunnerConfig(testJobImage, testToolCache))

	if got.Container == nil {
		t.Fatal("config declares no container section, so no tool cache is mounted")
	}
	if want := "--volume '" + testToolCache + ":" + RunnerToolCacheDir + "'"; got.Container.Options != want {
		t.Errorf("container options = %q, want %q", got.Container.Options, want)
	}
	if strings.Contains(got.Container.Options, RunnerDataDir+"/") {
		t.Errorf("container options use the runner's own view of the directory, not the host's: %q", got.Container.Options)
	}
	// The runner sanitizes every job container's binds against this
	// allowlist and always applies it, so a source missing from it is
	// dropped with a warning and the job runs on with no cache.
	if len(got.Container.ValidVolumes) != 1 || got.Container.ValidVolumes[0] != testToolCache {
		t.Errorf("valid_volumes = %v, want exactly [%q]", got.Container.ValidVolumes, testToolCache)
	}
}

// A host path with a space in it — the operator picks the remote directory —
// survives both the YAML file and the POSIX-shell splitting the runner
// applies to the options string before Docker's flag parser sees it.
func TestRenderRunnerConfigQuotesAToolCachePathWithASpace(t *testing.T) {
	const spaced = "/opt/my forge/runner/toolcache"
	got := decodeRunnerConfig(t, RenderRunnerConfig(testJobImage, spaced))

	if got.Container == nil {
		t.Fatal("config declares no container section")
	}
	if want := "--volume '" + spaced + ":" + RunnerToolCacheDir + "'"; got.Container.Options != want {
		t.Errorf("container options = %q, want %q", got.Container.Options, want)
	}
	if got.Container.ValidVolumes[0] != spaced {
		t.Errorf("valid_volumes = %v, want the unquoted path %q", got.Container.ValidVolumes, spaced)
	}
}

// No cache path, no container section — rather than a half-written mount or
// an empty stanza the runner has to interpret.
func TestRenderRunnerConfigOmitsTheContainerSectionWithNoToolCache(t *testing.T) {
	raw := RenderRunnerConfig(testJobImage, "")
	got := decodeRunnerConfig(t, raw)

	if got.Container != nil {
		t.Errorf("config declares a container section with no tool cache to mount:\n%s", raw)
	}
	if strings.Contains(string(raw), RunnerToolCacheDir) {
		t.Errorf("config names the tool cache mount point with nothing to mount:\n%s", raw)
	}
}

// The file is regenerated on every `up` and compared against what the host
// already has, so identical inputs must render identical bytes (UP-003).
func TestRenderRunnerConfigIsStable(t *testing.T) {
	first := RenderRunnerConfig(testJobImage, testToolCache)
	for i := 0; i < 5; i++ {
		if got := RenderRunnerConfig(testJobImage, testToolCache); !bytes.Equal(got, first) {
			t.Fatalf("render %d differs:\n%s\nwant\n%s", i, got, first)
		}
	}
	if !strings.HasPrefix(string(first), "# Generated by Farrier.") {
		t.Errorf("config does not say who generated it:\n%s", first)
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

	want := "docker compose exec -T -u git forgejo forgejo forgejo-cli actions register --secret-stdin --name 'farrier-colocated' --labels 'docker,ubuntu-latest' < '/opt/farrier/runner/secret'"
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
