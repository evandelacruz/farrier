package deploy

import (
	"context"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// quarantineOptions is testOptions plus the flag under test, so a
// quarantined deployment and the ordinary one it is compared against differ
// in exactly one input.
func quarantineOptions(remoteDir string) Options {
	opts := testOptions(remoteDir)
	opts.Quarantine = true
	return opts
}

func shippedAppINI(t *testing.T, host *fakeHost, remoteDir string) string {
	t.Helper()
	path := remoteDir + "/forge/app.ini"
	content, ok := host.files[path]
	if !ok {
		t.Fatalf("app.ini not shipped to %s, wrote: %v", path, keysOf(host.files))
	}
	return content
}

func shippedCompose(t *testing.T, host *fakeHost, remoteDir string) string {
	t.Helper()
	path := remoteDir + "/compose.tmp/docker-compose.yml"
	content, ok := host.files[path]
	if !ok {
		t.Fatalf("compose file not shipped to %s, wrote: %v", path, keysOf(host.files))
	}
	return content
}

// TestQuarantinedUpShipsAppINIWithOutboundDisabled is the config half of
// DRIL-002: the app.ini a quarantined deployment ships must leave the
// instance unable to send webhooks or email, whatever the restored database
// has configured.
func TestQuarantinedUpShipsAppINIWithOutboundDisabled(t *testing.T) {
	host := newFakeHost()

	if err := Up(context.Background(), events.NewJob(), host, testBundle(t), quarantineOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	appINI := shippedAppINI(t, host, "/opt/farrier")
	for _, want := range []string{
		"DISABLE_WEBHOOKS = true",
		"[webhook]\nALLOWED_HOST_LIST =",
		"[mailer]\nENABLED = false",
		"[mirror]\nENABLED = false",
	} {
		if !strings.Contains(appINI, want) {
			t.Errorf("quarantined app.ini missing %q:\n%s", want, appINI)
		}
	}
}

// TestQuarantinedUpPublishesHTTPSOnLoopbackOnly is DRIL-002's "reachable
// only through an SSH tunnel": Caddy's port is bound to the scratch host's
// loopback interface, so the only route in is a tunnel terminating there.
func TestQuarantinedUpPublishesHTTPSOnLoopbackOnly(t *testing.T) {
	host := newFakeHost()

	if err := Up(context.Background(), events.NewJob(), host, testBundle(t), quarantineOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	composed := shippedCompose(t, host, "/opt/farrier")
	if !strings.Contains(composed, orchestrate.LoopbackAddress+":443:443") {
		t.Errorf("quarantined compose does not bind caddy's https port to loopback: %s", composed)
	}
	// The bare "443:443" a normal deployment publishes would bind every
	// interface. It must not appear other than as the tail of the loopback
	// entry asserted above.
	if strings.Count(composed, "443:443") != strings.Count(composed, orchestrate.LoopbackAddress+":443:443") {
		t.Errorf("quarantined compose publishes caddy's https port on every interface: %s", composed)
	}
}

// TestUnquarantinedUpIsUnchanged pins the other half of "quarantine applies
// to drills only": an ordinary `up` renders and publishes exactly what it
// did before DRIL-002. A drilled instance being unreachable is the point; a
// production one being unreachable is an outage.
func TestUnquarantinedUpIsUnchanged(t *testing.T) {
	host := newFakeHost()

	if err := Up(context.Background(), events.NewJob(), host, testBundle(t), testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	appINI := shippedAppINI(t, host, "/opt/farrier")
	for _, unwanted := range []string{"DISABLE_WEBHOOKS", "[webhook]", "[mailer]", "[mirror]"} {
		if strings.Contains(appINI, unwanted) {
			t.Errorf("ordinary app.ini carries quarantine override %q:\n%s", unwanted, appINI)
		}
	}

	composed := shippedCompose(t, host, "/opt/farrier")
	if strings.Contains(composed, orchestrate.LoopbackAddress) {
		t.Errorf("ordinary compose binds caddy's https port to loopback: %s", composed)
	}
	if !strings.Contains(composed, "443:443") {
		t.Errorf("ordinary compose missing caddy's published https port: %s", composed)
	}
}

// TestQuarantineChangesTheAppINIChecksum guards the interaction between
// DRIL-002 and UP-003's convergence: `docker compose up -d` diffs a
// service's resolved config, never the bytes of a bind-mounted file, so the
// checksum deploy sets from the rendered app.ini (WithEnv, appINIChecksumEnv)
// is what makes a config-only change visible. Quarantine is a config-only
// change, so a host converged one way must actually be re-converged when
// converged the other.
func TestQuarantineChangesTheAppINIChecksum(t *testing.T) {
	ordinary := newFakeHost()
	if err := Up(context.Background(), events.NewJob(), ordinary, testBundle(t), testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}
	quarantined := newFakeHost()
	if err := Up(context.Background(), events.NewJob(), quarantined, testBundle(t), quarantineOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	before := checksumOf(t, shippedCompose(t, ordinary, "/opt/farrier"))
	after := checksumOf(t, shippedCompose(t, quarantined, "/opt/farrier"))
	if before == after {
		t.Errorf("%s is %q for both an ordinary and a quarantined deployment; converge cannot tell them apart", appINIChecksumEnv, before)
	}
}

func checksumOf(t *testing.T, composed string) string {
	t.Helper()
	for _, line := range strings.Split(composed, "\n") {
		if _, value, ok := strings.Cut(line, appINIChecksumEnv+":"); ok {
			return strings.TrimSpace(value)
		}
	}
	t.Fatalf("compose file carries no %s: %s", appINIChecksumEnv, composed)
	return ""
}
