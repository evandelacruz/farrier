package deploy

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// forgePorts returns the ports the converged Compose definition publishes on
// the forgejo service, so a test asserts on that service's own mapping
// rather than on a string appearing anywhere in the file.
func forgePorts(t *testing.T, host *fakeHost, remoteDir string) []string {
	t.Helper()
	services := convergedCompose(t, host, remoteDir)
	svc, ok := services[forge.Service].(map[string]any)
	if !ok {
		t.Fatalf("converged compose declares no %q service: %v", forge.Service, services)
	}
	raw, _ := svc["ports"].([]any)
	ports := make([]string, 0, len(raw))
	for _, p := range raw {
		ports = append(ports, fmt.Sprint(p))
	}
	return ports
}

// stepDetail returns the detail of step's succeeded event.
func stepDetail(t *testing.T, evs []events.Event, step string) string {
	t.Helper()
	for _, e := range evs {
		if e.Step == step && e.State == events.StateSucceeded {
			return e.Detail
		}
	}
	t.Fatalf("no succeeded event for step %q in %v", step, evs)
	return ""
}

// TestUpPublishesGitSSHOnTheDefaultPort is UP-005's default case: a bundle
// that names no port comes up with the forgejo service's builtin SSH server
// published on host port 2222, so `git clone` and `git push` over SSH work
// against a fresh deployment without the operator configuring anything.
func TestUpPublishesGitSSHOnTheDefaultPort(t *testing.T) {
	host := newFakeHost()

	if err := Up(context.Background(), events.NewJob(), host, testBundle(t), testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	want := fmt.Sprintf("%d:%d", bundle.DefaultGitSSHPort, forge.SSHListenPort)
	if ports := forgePorts(t, host, "/opt/farrier"); !slices.Contains(ports, want) {
		t.Errorf("forgejo publishes %v, want git over ssh as %s", ports, want)
	}

	appINI := shippedAppINI(t, host, "/opt/farrier")
	if !strings.Contains(appINI, fmt.Sprintf("SSH_PORT = %d\n", bundle.DefaultGitSSHPort)) {
		t.Errorf("app.ini advertises a port other than the published default:\n%s", appINI)
	}
}

// TestUpPublishesTheManifestSSHPort is the other half: an operator whose
// host sshd does not own 22 sets it in the manifest and gets bare
// `git@domain:owner/repo.git` clone URLs — the published host port and the
// port app.ini advertises are the same number, read from the same field, so
// the URL Forgejo displays is the one that answers.
func TestUpPublishesTheManifestSSHPort(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	b.Manifest.GitSSHPort = 22

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	ports := forgePorts(t, host, "/opt/farrier")
	want := fmt.Sprintf("22:%d", forge.SSHListenPort)
	if !slices.Contains(ports, want) {
		t.Errorf("forgejo publishes %v, want the manifest's %s", ports, want)
	}
	if def := fmt.Sprintf("%d:%d", bundle.DefaultGitSSHPort, forge.SSHListenPort); slices.Contains(ports, def) {
		t.Errorf("forgejo publishes the default %s against a manifest that declares 22: %v", def, ports)
	}

	appINI := shippedAppINI(t, host, "/opt/farrier")
	if !strings.Contains(appINI, "SSH_PORT = 22\n") {
		t.Errorf("app.ini advertises a different port than the one published:\n%s", appINI)
	}
}

// TestUpServesGitSSHWithTheBundleHostKey ties UP-005's two halves together:
// the port is published on the same service the bundle's own SSH host key
// was installed for (RSTR-004), so what answers there is the instance's
// permanent identity rather than a key the container generated for itself.
// Nothing in either half is first-deploy-specific or host-specific, which is
// what makes a restored or promoted instance — deployed through this same Up
// from the same bundle — answer on the same port with the same key.
func TestUpServesGitSSHWithTheBundleHostKey(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	b.Manifest.GitSSHPort = 2022

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	keyPath := GiteaStatePath("/opt/farrier") + "/" + sshHostKeyRelPath()
	if _, ok := host.files[keyPath]; !ok {
		t.Fatalf("bundle ssh host key not installed at %s, wrote: %v", keyPath, keysOf(host.files))
	}

	appINI := shippedAppINI(t, host, "/opt/farrier")
	if !strings.Contains(appINI, "SSH_SERVER_HOST_KEYS = "+forge.SSHHostKeyPath) {
		t.Errorf("app.ini does not pin forgejo to the bundle's host key:\n%s", appINI)
	}
	if ports := forgePorts(t, host, "/opt/farrier"); !slices.Contains(ports, fmt.Sprintf("2022:%d", forge.SSHListenPort)) {
		t.Errorf("git over ssh is not published on the service holding the bundle key: %v", ports)
	}
}

// TestUpReportsTheCloneURL checks the operator is told where to point a
// remote, through the same CORE-002 event stream both frontends render
// (XCUT-002) — and that no key material rides along with it (KEY-003).
func TestUpReportsTheCloneURL(t *testing.T) {
	cases := []struct {
		name string
		port int
		want string
	}{
		{"default port carries the port in the url", 0, "ssh://git@example.com:2222/"},
		{"port 22 renders scp-style", 22, "git@example.com:<owner>/<repo>.git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := newFakeHost()
			b := testBundle(t)
			b.Manifest.GitSSHPort = tc.port
			job := events.NewJob()

			if err := Up(context.Background(), job, host, b, testOptions("/opt/farrier")); err != nil {
				t.Fatalf("Up: %v", err)
			}

			detail := stepDetail(t, drain(job), StepConfigureGitSSH)
			if !strings.Contains(detail, tc.want) {
				t.Errorf("git-over-ssh event detail = %q, want it to name %q", detail, tc.want)
			}
			if strings.Contains(detail, "PRIVATE KEY") {
				t.Errorf("git-over-ssh event detail leaks key material: %q", detail)
			}
		})
	}
}

// TestQuarantinedUpPublishesGitSSHOnLoopbackOnly extends DRIL-002 to the
// port UP-005 adds: a drilled instance carries production's SSH host key,
// so a routable git-over-SSH port on the scratch host would be a second
// endpoint answering as production.
func TestQuarantinedUpPublishesGitSSHOnLoopbackOnly(t *testing.T) {
	host := newFakeHost()

	if err := Up(context.Background(), events.NewJob(), host, testBundle(t), quarantineOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	ports := forgePorts(t, host, "/opt/farrier")
	mapping := fmt.Sprintf("%d:%d", bundle.DefaultGitSSHPort, forge.SSHListenPort)
	if !slices.Contains(ports, orchestrate.LoopbackAddress+":"+mapping) {
		t.Errorf("quarantined forgejo publishes %v, want git over ssh bound to loopback", ports)
	}
	if slices.Contains(ports, mapping) {
		t.Errorf("quarantined forgejo publishes git over ssh on every interface: %v", ports)
	}
}
