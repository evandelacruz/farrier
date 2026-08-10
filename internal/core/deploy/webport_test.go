package deploy

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/caddy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// The defect these tests exist for: `up` published Caddy on 80 or 443
// unconditionally, and a host already serving something there refused the
// deployment outright — "Bind for 0.0.0.0:80 failed: port is already
// allocated". The operator brings the host, so the published port is
// theirs (bundle.Manifest.WebPort) and only the host side of the mapping
// moves.

// A named bundle that names a port publishes onto it, and the container
// side stays where Caddy actually binds.
func TestUpPublishesTheManifestWebPort(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	b.Manifest.WebPort = 8443
	b.Manifest.PublicWebPort = 443

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	want := fmt.Sprintf("8443:%d", caddy.HTTPSPort)
	if got := caddyPorts(t, host, "/opt/farrier"); len(got) != 1 || got[0] != want {
		t.Errorf("caddy ports = %v, want [%s]", got, want)
	}
}

// A nameless bundle that names none takes 8222 rather than 80, which is the
// whole reason the nameless tier stopped colliding with whatever else the
// operator's own machine is already serving.
func TestUpPublishesANamelessBundleOffPort80ByDefault(t *testing.T) {
	host := newFakeHost()

	if err := Up(context.Background(), events.NewJob(), host, namelessBundle(t), namelessOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	ports := caddyPorts(t, host, "/opt/farrier")
	want := fmt.Sprintf("%d:%d", bundle.DefaultNamelessWebPort, caddy.HTTPPort)
	if len(ports) != 1 || ports[0] != want {
		t.Errorf("caddy ports = %v, want [%s]", ports, want)
	}
	for _, p := range ports {
		if strings.HasPrefix(p, "80:") {
			t.Errorf("caddy still publishes host port 80 (%q) — the contended port the nameless default exists to avoid", p)
		}
	}
}

// The published port and the public one reach every URL the instance
// renders. ROOT_URL is the one that matters most: Forgejo builds the clone
// URLs it displays and the links it emails from it, so a wrong port here is
// a forge that runs while everything it tells people is unreachable.
func TestUpRendersThePublicPortIntoEveryURLItBuilds(t *testing.T) {
	for _, tc := range []struct {
		name          string
		webPort       int
		publicWebPort int
		wantURL       string
	}{
		{name: "nothing fronting it", webPort: 8443, publicWebPort: 8443, wantURL: "https://example.com:8443/"},
		{name: "a proxy on 443 forwards to it", webPort: 8443, publicWebPort: 443, wantURL: "https://example.com/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newFakeHost()
			job := events.NewJob()
			b := runnerBundle(t)
			b.Manifest.WebPort = tc.webPort
			b.Manifest.PublicWebPort = tc.publicWebPort

			if err := Up(context.Background(), job, host, b, testOptions("/opt/farrier")); err != nil {
				t.Fatalf("Up: %v", err)
			}
			evs := drain(job)

			appINI := host.files["/opt/farrier/"+hostConfigDir+"/"+appINIFilename]
			if !strings.Contains(appINI, "ROOT_URL = "+tc.wantURL) {
				t.Errorf("app.ini ROOT_URL is not %s:\n%s", tc.wantURL, appINI)
			}
			// DOMAIN is a host, not a URL, and never carries a port.
			if !strings.Contains(appINI, "DOMAIN = example.com\n") {
				t.Errorf("app.ini DOMAIN is not the bare domain:\n%s", appINI)
			}

			svc, ok := convergedCompose(t, host, "/opt/farrier")[forge.RunnerService].(map[string]any)
			if !ok {
				t.Fatalf("converged compose declares no %q service", forge.RunnerService)
			}
			if command := fmt.Sprint(svc["command"]); !strings.Contains(command, tc.wantURL) {
				t.Errorf("runner registers against something other than %s:\n%s", tc.wantURL, command)
			}

			if ready := stepDetail(t, evs, StepWaitCaddy); !strings.Contains(ready, tc.wantURL) {
				t.Errorf("wait-caddy detail = %q, want the URL clients actually use (%s)", ready, tc.wantURL)
			}
		})
	}
}

// Moving a named bundle's published port without saying where clients
// connect is refused before the host is touched at all. Farrier cannot see
// whether something is forwarding to it, and deploying on a guess is worse
// than refusing: the forge comes up healthy and every URL it hands out is
// wrong.
func TestUpRefusesANamedBundleOnANonStandardPortWithNoPublicPort(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	b.Manifest.WebPort = 8443

	err := Up(context.Background(), events.NewJob(), host, b, testOptions("/opt/farrier"))
	if err == nil {
		t.Fatal("Up: want a refusal, got nil")
	}
	if !strings.Contains(err.Error(), "publicWebPort") {
		t.Errorf("refusal = %q, want it to name the field that resolves it", err)
	}
	if len(host.files) != 0 {
		t.Errorf("a refused deployment wrote %d files to the host; it must leave the host exactly as it found it", len(host.files))
	}
	if len(host.commands) != 0 {
		t.Errorf("a refused deployment ran commands on the host: %v", host.commands)
	}
}

// A drilled instance publishes on the manifest's port too — bound to
// loopback (DRIL-002) — but its runner keeps reaching Caddy at the
// container port, because the Docker network alias resolves the domain
// inside the network where no host-side mapping applies.
func TestQuarantinePublishesTheManifestPortAndLeavesTheRunnerOnTheContainerPort(t *testing.T) {
	host := newFakeHost()
	b := runnerBundle(t)
	b.Manifest.WebPort = 8443
	b.Manifest.PublicWebPort = 8443

	opts := testOptions("/opt/farrier")
	opts.Quarantine = true
	if err := Up(context.Background(), events.NewJob(), host, b, opts); err != nil {
		t.Fatalf("Up: %v", err)
	}

	want := fmt.Sprintf("%s:8443:%d", orchestrate.LoopbackAddress, caddy.HTTPSPort)
	if got := caddyPorts(t, host, "/opt/farrier"); len(got) != 1 || got[0] != want {
		t.Errorf("caddy ports = %v, want [%s]", got, want)
	}

	svc, ok := convergedCompose(t, host, "/opt/farrier")[forge.RunnerService].(map[string]any)
	if !ok {
		t.Fatalf("converged compose declares no %q service", forge.RunnerService)
	}
	command := fmt.Sprint(svc["command"])
	if !strings.Contains(command, "https://example.com/") {
		t.Errorf("drilled runner is not pointed at the aliased domain on caddy's container port:\n%s", command)
	}
	if strings.Contains(command, ":8443") {
		t.Errorf("drilled runner reaches for the published host port, which nothing inside the network is listening on:\n%s", command)
	}
}

// The address is where the instance is reached, not how it is published:
// the port is bundle content that travels with the bundle, so an address
// carrying one is refused with a message that says where the port lives.
func TestNormalizeAddressPointsAPortAtTheManifest(t *testing.T) {
	_, err := NormalizeAddress("box.tail1234.ts.net:8222")
	if err == nil {
		t.Fatal("NormalizeAddress: want an error for an address with a port, got nil")
	}
	if !strings.Contains(err.Error(), "webPort") {
		t.Errorf("error = %q, want it to name the manifest field the port belongs in", err)
	}
}
