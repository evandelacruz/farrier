package drill

import (
	"context"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/caddy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"gopkg.in/yaml.v3"
)

// The tests here are DRIL-002's acceptance bar, driven end to end through
// the real Drill rather than through the Options it hands restore: what
// matters is what actually lands on the scratch target, and no caller of
// Drill can opt out of any of it.

func drilledFile(t *testing.T, host *fakeHost, path string) string {
	t.Helper()
	host.mu.Lock()
	defer host.mu.Unlock()
	content, ok := host.files[path]
	if !ok {
		paths := make([]string, 0, len(host.files))
		for p := range host.files {
			paths = append(paths, p)
		}
		t.Fatalf("drill never shipped %s; shipped: %v", path, paths)
	}
	return content
}

// TestDrilledInstanceCannotSendWebhooksOrEmail is DRIL-002's first
// property. The restored database is production's, so it holds
// production's webhook targets and mailer settings; the app.ini the drill
// ships is what makes the instance unable to act on any of them.
func TestDrilledInstanceCannotSendWebhooksOrEmail(t *testing.T) {
	f := newFixture(t)
	if _, err := Drill(context.Background(), events.NewJob(), f.opts); err != nil {
		t.Fatalf("Drill: %v", err)
	}

	appINI := drilledFile(t, f.host(), "/opt/farrier-drill/forge/app.ini")
	for _, tc := range []struct {
		door string
		want string
	}{
		{"repository and system webhooks", "DISABLE_WEBHOOKS = true"},
		{"webhook delivery hosts", "ALLOWED_HOST_LIST ="},
		{"outbound email", "[mailer]\nENABLED = false"},
		{"push mirrors", "[mirror]\nENABLED = false"},
	} {
		if !strings.Contains(appINI, tc.want) {
			t.Errorf("drilled app.ini leaves %s open (want %q):\n%s", tc.door, tc.want, appINI)
		}
	}
}

// TestDrilledInstanceIsReachableOnlyThroughAnSSHTunnel is DRIL-002's third
// property. Caddy's HTTPS port is bound to the scratch host's loopback
// interface, so the drilled instance answers a tunnel terminating on that
// host and nothing routable — a bare "443:443" would publish it on every
// interface, at production's own certificate, to anyone who found the
// scratch host's address.
func TestDrilledInstanceIsReachableOnlyThroughAnSSHTunnel(t *testing.T) {
	f := newFixture(t)
	if _, err := Drill(context.Background(), events.NewJob(), f.opts); err != nil {
		t.Fatalf("Drill: %v", err)
	}

	composed := drilledFile(t, f.host(), "/opt/farrier-drill/compose.tmp/docker-compose.yml")
	loopback := orchestrate.LoopbackAddress + ":443:443"
	if !strings.Contains(composed, loopback) {
		t.Errorf("drilled compose does not bind caddy's https port to loopback:\n%s", composed)
	}
	if strings.Count(composed, "443:443") != strings.Count(composed, loopback) {
		t.Errorf("drilled compose publishes caddy's https port on every interface:\n%s", composed)
	}
}

// TestDrilledInstanceKeepsTheSnapshotsIdentity pins what quarantine must
// not change. A drill exists to prove the snapshot restores; an instance
// rendered with different secrets, a different domain, or a different SSH
// host key would prove something else. Quarantine constrains what the
// instance may do, never who it is (spec.md "Rehearsal").
func TestDrilledInstanceKeepsTheSnapshotsIdentity(t *testing.T) {
	f := newFixture(t)
	if _, err := Drill(context.Background(), events.NewJob(), f.opts); err != nil {
		t.Fatalf("Drill: %v", err)
	}

	appINI := drilledFile(t, f.host(), "/opt/farrier-drill/forge/app.ini")
	values := testKeyValues()
	for _, want := range []string{
		"DOMAIN = " + testDomain,
		"ROOT_URL = https://" + testDomain + "/",
		"SECRET_KEY = " + values[forge.KeySecretKey],
		"INSTALL_LOCK = true",
		// Actions stays on: a drill has to be able to run CI (DRIL-001).
		"[actions]\nENABLED = true",
	} {
		if !strings.Contains(appINI, want) {
			t.Errorf("drilled app.ini missing %q; quarantine changed the instance's identity:\n%s", want, appINI)
		}
	}
}

// TestDrilledRunnerReachesTheDrilledInstance is the property the smoke job
// (DRIL-001) depends on and quarantine owns. The colocated runner connects
// to the bundle domain, and a drill leaves DNS pointing that domain at
// production — so on the drilled host the domain has to resolve to the
// drilled Caddy on the deployment's own Docker network. Without the alias
// the drilled runner would attach to production's job queue, holding the
// same runner secret production's own runner holds, and run production's CI
// on the scratch target.
func TestDrilledRunnerReachesTheDrilledInstance(t *testing.T) {
	f := newFixture(t)
	if _, err := Drill(context.Background(), events.NewJob(), f.opts); err != nil {
		t.Fatalf("Drill: %v", err)
	}

	composed := drilledFile(t, f.host(), "/opt/farrier-drill/compose.tmp/docker-compose.yml")

	// Decoded generically: only Caddy is in Compose's mapping form for
	// networks, every other service keeping the list form Render emits.
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(composed), &doc); err != nil {
		t.Fatalf("parse drilled compose: %v\n%s", err, composed)
	}
	services, _ := doc["services"].(map[string]any)
	service, _ := services[caddy.Service].(map[string]any)
	networks, ok := service["networks"].(map[string]any)
	if !ok {
		t.Fatalf("drilled caddy declares no per-network options, so it carries no alias:\n%s", composed)
	}

	var aliased bool
	for _, options := range networks {
		opts, _ := options.(map[string]any)
		aliases, _ := opts["aliases"].([]any)
		for _, alias := range aliases {
			if alias == testDomain {
				aliased = true
			}
		}
	}
	if !aliased {
		t.Errorf("drilled caddy does not answer to %s on the deployment network:\n%s", testDomain, composed)
	}

	// The runner's own configuration is untouched: it still connects to the
	// bundle domain over HTTPS, exactly as production's does. The alias
	// changes where that resolves on the drilled host, nothing else.
	if !strings.Contains(composed, forge.RunnerInstanceURL(testDomain)) {
		t.Errorf("drilled runner no longer connects to the bundle domain:\n%s", composed)
	}
}
