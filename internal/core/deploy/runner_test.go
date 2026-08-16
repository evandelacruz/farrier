package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"gopkg.in/yaml.v3"
)

// testRunnerSecret is a well-formed runner secret (40 lowercase hex
// characters, forge.ValidateRunnerSecret) for tests that need one in the
// keystore.
const testRunnerSecret = "0123456789abcdef0123456789abcdef01234567"

// runnerBundle returns testBundle's bundle extended into one that actually
// carries a colocated runner: the pinned image, the rendered service, and
// the runner secret in the bundle's own keystore directory.
func runnerBundle(t *testing.T) *bundle.Bundle {
	t.Helper()
	b := testBundle(t)
	b.Manifest.Images[forge.RunnerService] = "code.forgejo.org/forgejo/runner@sha256:" + strings.Repeat("c", 64)
	b.Compose = map[string][]byte{
		"docker-compose.yml": []byte("services:\n  forgejo:\n    image: x\n  caddy:\n    image: y\n  runner:\n    image: z\n"),
	}

	keysDir, _ := b.Manifest.Drivers.Keystore.Config["path"].(string)
	if keysDir == "" {
		t.Fatal("test bundle has no keystore path")
	}
	if err := os.WriteFile(filepath.Join(keysDir, forge.KeyRunnerSecret), []byte(testRunnerSecret), 0o600); err != nil {
		t.Fatalf("write runner secret fixture: %v", err)
	}
	return b
}

// convergedCompose decodes the Compose definition Up shipped to the host —
// the one Converge wrote, after every deploy-time layering step. Converge
// writes into a staging directory and moves it into place with a shell
// command the fake host records but does not perform, so the shipped bytes
// are found under compose.tmp/ here.
func convergedCompose(t *testing.T, host *fakeHost, remoteDir string) map[string]any {
	t.Helper()
	raw, ok := host.files[filepath.Join(remoteDir, "compose.tmp", orchestrate.ComposeFile)]
	if !ok {
		t.Fatalf("no compose file shipped; host has %v", hostPaths(host))
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decode shipped compose: %v", err)
	}
	services, _ := doc["services"].(map[string]any)
	return services
}

func hostPaths(host *fakeHost) []string {
	paths := make([]string, 0, len(host.files))
	for p := range host.files {
		paths = append(paths, p)
	}
	return paths
}

func TestUpDeploysAndRegistersTheColocatedRunner(t *testing.T) {
	host := newFakeHost()
	b := runnerBundle(t)

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	secret, ok := host.files[RunnerSecretPath("/opt/farrier")]
	if !ok {
		t.Fatalf("runner secret not shipped to %s; host has %v", RunnerSecretPath("/opt/farrier"), hostPaths(host))
	}
	if secret != testRunnerSecret {
		t.Errorf("shipped secret = %q, want the bundle's", secret)
	}

	config, ok := host.files[RunnerConfigPath("/opt/farrier")]
	if !ok {
		t.Fatalf("runner config not shipped to %s; host has %v", RunnerConfigPath("/opt/farrier"), hostPaths(host))
	}
	want := string(forge.RenderRunnerConfig(bundle.DefaultActionsJobImage, RunnerToolCachePath("/opt/farrier")))
	if config != want {
		t.Errorf("shipped config =\n%s\nwant\n%s", config, want)
	}

	services := convergedCompose(t, host, "/opt/farrier")
	svc, ok := services[forge.RunnerService].(map[string]any)
	if !ok {
		t.Fatalf("converged compose declares no %q service: %v", forge.RunnerService, services)
	}

	volumes := stringsOf(svc["volumes"])
	wantMounts := []string{
		RunnerHostPath("/opt/farrier") + ":" + forge.RunnerDataDir,
		forge.DockerSocketPath + ":" + forge.DockerSocketPath,
	}
	for _, want := range wantMounts {
		if !containsString(volumes, want) {
			t.Errorf("runner volumes %v missing %q", volumes, want)
		}
	}

	if user, _ := svc["user"].(string); user != forge.RunnerUser {
		t.Errorf("runner user = %q, want %q", user, forge.RunnerUser)
	}

	env, _ := svc["environment"].(map[string]any)
	if got, _ := env[forge.DockerHostEnv].(string); got != forge.DockerHostValue {
		t.Errorf("%s = %q, want %q", forge.DockerHostEnv, got, forge.DockerHostValue)
	}

	command := stringsOf(svc["command"])
	joined := strings.Join(command, " ")
	if !strings.Contains(joined, "forgejo-runner daemon") {
		t.Errorf("runner command %v does not start the daemon", command)
	}
	if !strings.Contains(joined, "-c '"+forge.RunnerConfigFilename+"'") {
		t.Errorf("runner command %v does not pass the shipped config", command)
	}

	if !ranCommandContaining(host, "forgejo-cli actions register") {
		t.Errorf("no registration command ran; commands: %v", host.commands)
	}
	register := commandContaining(host, "forgejo-cli actions register")
	if !strings.Contains(register, "--labels 'docker,ubuntu-latest'") {
		t.Errorf("registration does not declare labels: %s", register)
	}
}

// The tool cache directory has to exist, and be writable by whatever uid a
// job image runs as, before the first job mounts it. Docker would create a
// missing bind source itself — root-owned, which a non-root job image cannot
// write, leaving every run re-downloading its toolchain with nothing saying
// so.
func TestUpCreatesTheRunnerToolCacheDirectoryOnTheHost(t *testing.T) {
	host := newFakeHost()
	b := runnerBundle(t)

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	cache := RunnerToolCachePath("/opt/farrier")
	if cache != "/opt/farrier/runner/toolcache" {
		t.Errorf("tool cache path = %q, want it under the remote directory's runner directory", cache)
	}
	mkdir := commandContaining(host, "mkdir -p '"+cache+"'")
	if mkdir == "" {
		t.Errorf("no command created %s; commands: %v", cache, host.commands)
	}
	if !ranCommandContaining(host, "chmod 0777 '"+cache+"'") {
		t.Errorf("tool cache directory is not made writable; commands: %v", host.commands)
	}

	created := indexOfCommandContaining(host, "mkdir -p '"+cache+"'")
	converge := indexOfCommandContaining(host, "docker compose up")
	if created < 0 || converge < 0 || created > converge {
		t.Errorf("tool cache created at %d, converge at %d; want it to exist before any container starts", created, converge)
	}
}

// The seam this change lives on: the runner reaches the *host's* Docker
// daemon over the mounted socket, so the volume source the job container
// gets has to be the path as the host sees it. The runner's own view of the
// same directory — under forge.RunnerDataDir, where this sits — is a
// container path the host daemon has never heard of, and it produces a mount
// of the wrong thing rather than a failure at job start.
func TestUpMountsTheToolCacheIntoJobContainersByHostPath(t *testing.T) {
	host := newFakeHost()
	b := runnerBundle(t)

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	config := host.files[RunnerConfigPath("/opt/farrier")]
	cache := RunnerToolCachePath("/opt/farrier")

	var got struct {
		Container *struct {
			Options      string   `yaml:"options"`
			ValidVolumes []string `yaml:"valid_volumes"`
		} `yaml:"container"`
	}
	if err := yaml.Unmarshal([]byte(config), &got); err != nil {
		t.Fatalf("shipped runner config is not YAML: %v\n%s", err, config)
	}
	if got.Container == nil {
		t.Fatalf("shipped runner config mounts no tool cache:\n%s", config)
	}
	if want := "--volume '" + cache + ":" + forge.RunnerToolCacheDir + "'"; got.Container.Options != want {
		t.Errorf("container options = %q, want %q", got.Container.Options, want)
	}
	if !containsString(got.Container.ValidVolumes, cache) {
		t.Errorf("valid_volumes = %v, want the host path %q; the runner drops binds missing from it", got.Container.ValidVolumes, cache)
	}
	// The container-side spelling of the same directory. Mounting by it
	// would resolve against the host daemon's filesystem, where it names
	// something else or nothing.
	if containerView := forge.RunnerDataDir + "/toolcache"; strings.Contains(config, containerView) {
		t.Errorf("shipped config names the runner's own view %q instead of the host path %q:\n%s", containerView, cache, config)
	}
}

// The cache is rebuildable from the network and grows without bound, so it
// must not reach a snapshot. Backup reads git out of GitStatePath, the
// database through the forgejo container, blobs through the blob driver, and
// key material through the keystore — the only path-shaped exporter is the
// first — so staying outside the state directory is what keeps it out.
func TestRunnerToolCacheIsOutsideCapturedState(t *testing.T) {
	const remoteDir = "/opt/farrier"
	cache := RunnerToolCachePath(remoteDir)

	for _, captured := range []string{
		GitStatePath(remoteDir),
		GiteaStatePath(remoteDir),
		BlobsStatePath(remoteDir),
		filepath.Join(remoteDir, stateDir),
	} {
		if strings.HasPrefix(cache, captured+"/") || cache == captured {
			t.Errorf("tool cache %q sits under %q, which a snapshot captures", cache, captured)
		}
	}
}

// The job-container image is the manifest's. An operator who wants closer
// GitHub parity edits farrier.yaml and the next `up` ships it.
func TestUpUsesTheManifestJobImageForTheRunner(t *testing.T) {
	host := newFakeHost()
	b := runnerBundle(t)
	const image = "ghcr.io/catthehacker/ubuntu:act-22.04"
	b.Manifest.Actions.JobImage = image

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	config := host.files[RunnerConfigPath("/opt/farrier")]
	for _, label := range forge.RunnerLabelNames {
		if want := label + ":docker://" + image; !strings.Contains(config, want) {
			t.Errorf("shipped config missing %q:\n%s", want, config)
		}
	}
	if strings.Contains(config, bundle.DefaultActionsJobImage) {
		t.Errorf("shipped config still carries the default image:\n%s", config)
	}
}

// A manifest that names no job image — one written before the field existed,
// or one an operator never touched — gets the default, which is what the
// constant this replaced always held.
func TestUpFallsBackToTheDefaultJobImage(t *testing.T) {
	host := newFakeHost()
	b := runnerBundle(t)
	b.Manifest.Actions.JobImage = ""

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	config := host.files[RunnerConfigPath("/opt/farrier")]
	for _, label := range forge.RunnerLabelNames {
		if want := label + ":docker://" + bundle.DefaultActionsJobImage; !strings.Contains(config, want) {
			t.Errorf("shipped config missing %q:\n%s", want, config)
		}
	}
}

// The runner has to be registered against a Forgejo that is already up, so
// the ordering is: converge, wait for forgejo, then register.
func TestUpRegistersTheRunnerAfterForgejoIsReady(t *testing.T) {
	host := newFakeHost()
	b := runnerBundle(t)

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	converge := indexOfCommandContaining(host, "docker compose up")
	ready := indexOfCommandContaining(host, "exec -T forgejo true")
	register := indexOfCommandContaining(host, "forgejo-cli actions register")
	if converge < 0 || ready < 0 || register < 0 {
		t.Fatalf("missing a step; commands: %v", host.commands)
	}
	if !(converge < ready && ready < register) {
		t.Errorf("order was converge=%d ready=%d register=%d, want converge < ready < register", converge, ready, register)
	}
}

// KEY-003: the runner secret is key material. It may reach a 0600 file on
// the host and the registration command's stdin, and nothing else — not a
// command line, not an event.
func TestUpKeepsTheRunnerSecretOutOfCommandsAndEvents(t *testing.T) {
	host := newFakeHost()
	b := runnerBundle(t)
	job := events.NewJob()

	if err := Up(context.Background(), job, host, b, testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	for _, command := range host.commands {
		if strings.Contains(command, testRunnerSecret) {
			t.Errorf("command leaks the runner secret: %s", command)
		}
	}
	for _, ev := range job.Events() {
		if strings.Contains(ev.Detail, testRunnerSecret) {
			t.Errorf("event leaks the runner secret: %+v", ev)
		}
	}
	for path, content := range host.files {
		if path == RunnerSecretPath("/opt/farrier") {
			continue
		}
		if strings.Contains(content, testRunnerSecret) {
			t.Errorf("%s carries the runner secret", path)
		}
	}
}

// UP-003: a second `up` against the same host ships the same secret and
// runs the same registration — an upsert keyed by it — rather than minting
// a second runner.
func TestUpRepeatedLeavesOneRunnerRegistration(t *testing.T) {
	host := newFakeHost()
	b := runnerBundle(t)
	// reuse, so the certificate step does not persist a fake PEM the
	// second run would then fail to parse — this test is about the runner.
	opts := Options{RemoteDir: "/opt/farrier", CertIssuer: &fakeCertIssuer{reuse: true}}

	if err := Up(context.Background(), events.NewJob(), host, b, opts); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	first := host.files[RunnerSecretPath("/opt/farrier")]
	firstRegister := commandContaining(host, "forgejo-cli actions register")

	host.commands = nil
	if err := Up(context.Background(), events.NewJob(), host, b, opts); err != nil {
		t.Fatalf("second Up: %v", err)
	}

	if got := host.files[RunnerSecretPath("/opt/farrier")]; got != first {
		t.Errorf("second up rewrote the secret: %q, want %q", got, first)
	}
	second := commandContaining(host, "forgejo-cli actions register")
	if second != firstRegister {
		t.Errorf("registration command changed between runs:\n%s\n%s", firstRegister, second)
	}
	if strings.Count(strings.Join(host.commands, "\n"), "forgejo-cli actions register") != 1 {
		t.Errorf("registration ran %d times in one up; commands: %v",
			strings.Count(strings.Join(host.commands, "\n"), "forgejo-cli actions register"), host.commands)
	}
}

// The escape hatch spec.md "CI trust boundary" depends on: turning the
// colocated runner off takes the service out of the converged definition,
// so Converge's --remove-orphans takes down a runner an earlier deploy
// started, and nothing is registered or shipped for it.
func TestUpWithoutColocatedRunnerRemovesTheServiceEntirely(t *testing.T) {
	host := newFakeHost()
	b := runnerBundle(t)
	disabled := false
	b.Manifest.Actions.ColocatedRunner = &disabled

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	services := convergedCompose(t, host, "/opt/farrier")
	if _, ok := services[forge.RunnerService]; ok {
		t.Error("converged compose still declares the runner service")
	}
	if _, ok := host.files[RunnerSecretPath("/opt/farrier")]; ok {
		t.Error("runner secret was shipped for a deployment with no colocated runner")
	}
	if _, ok := host.files[RunnerConfigPath("/opt/farrier")]; ok {
		t.Error("runner config was shipped for a deployment with no colocated runner")
	}
	if ranCommandContaining(host, "forgejo-cli actions register") {
		t.Errorf("registered a runner that isn't deployed; commands: %v", host.commands)
	}
	// Nothing changes on a host with the runner turned off: no tool cache
	// directory, and nothing mounting one.
	cache := RunnerToolCachePath("/opt/farrier")
	if ranCommandContaining(host, cache) {
		t.Errorf("touched the tool cache directory for a deployment with no runner; commands: %v", host.commands)
	}
	for path, content := range host.files {
		if strings.Contains(content, forge.RunnerToolCacheDir) {
			t.Errorf("%s mounts a tool cache for a deployment with no runner", path)
		}
	}
}

// A manifest that asks for a colocated runner and pins no image for it is a
// contradiction, and fails loudly rather than quietly deploying no CI.
func TestUpFailsWhenAColocatedRunnerIsAskedForWithNoImage(t *testing.T) {
	host := newFakeHost()
	b := runnerBundle(t)
	delete(b.Manifest.Images, forge.RunnerService)
	enabled := true
	b.Manifest.Actions.ColocatedRunner = &enabled

	err := Up(context.Background(), events.NewJob(), host, b, testOptions("/opt/farrier"))
	if err == nil {
		t.Fatal("Up succeeded, want error")
	}
	if !strings.Contains(err.Error(), forge.RunnerService) {
		t.Errorf("error %q does not name the missing image", err)
	}
}

// A manifest that predates the field states no preference. Deploying it
// still works; the event stream says the deployment has no runner rather
// than the deployment failing.
func TestUpSkipsTheRunnerWhenTheManifestNeverMentionsIt(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t) // no runner image, no actions section
	job := events.NewJob()

	if err := Up(context.Background(), job, host, b, testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if ranCommandContaining(host, "forgejo-cli actions register") {
		t.Errorf("registered a runner the bundle does not pin; commands: %v", host.commands)
	}

	var detail string
	for _, ev := range job.Events() {
		if ev.Step == StepConfigureRunner && ev.State == events.StateSucceeded {
			detail = ev.Detail
		}
	}
	if !strings.Contains(detail, "no colocated runner") {
		t.Errorf("configure-runner detail = %q, want it to say the deployment has no runner", detail)
	}
}

func stringsOf(value any) []string {
	entries, _ := value.([]any)
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if s, ok := entry.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func ranCommandContaining(host *fakeHost, substr string) bool {
	return indexOfCommandContaining(host, substr) >= 0
}

func indexOfCommandContaining(host *fakeHost, substr string) int {
	for i, command := range host.commands {
		if strings.Contains(command, substr) {
			return i
		}
	}
	return -1
}

func commandContaining(host *fakeHost, substr string) string {
	if i := indexOfCommandContaining(host, substr); i >= 0 {
		return host.commands[i]
	}
	return ""
}
