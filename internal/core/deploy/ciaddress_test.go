package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
)

// The tests here cover the refusal a live deployment earned: `up -address
// 127.0.0.1` on the operator's own macOS machine reported every step green,
// registered the runner, and produced an instance whose CI could never run
// — the runner container restarting forever against its own loopback, the
// first workflow queued with no error anywhere an operator looks.

// errAddressDiscovery stands in for a host with neither `ip` nor
// `ifconfig`, so the question "what address are you reachable at" has no
// answer at all.
var errAddressDiscovery = errors.New("sh: ip: not found")

// ipAddrOutput is `ip -o addr show` on a Linux host: loopback, a real LAN
// interface, and the Docker bridge the host manages itself.
const ipAddrOutput = `1: lo    inet 127.0.0.1/8 scope host lo\       valid_lft forever preferred_lft forever
1: lo    inet6 ::1/128 scope host \       valid_lft forever preferred_lft forever
2: eth0    inet 192.168.1.5/24 brd 192.168.1.255 scope global eth0\       valid_lft forever preferred_lft forever
2: eth0    inet6 fd00::5/64 scope global \       valid_lft forever preferred_lft forever
2: eth0    inet6 fe80::42:acff:fe11:2/64 scope link \       valid_lft forever preferred_lft forever
3: docker0    inet 172.17.0.1/16 brd 172.17.255.255 scope global docker0\       valid_lft forever preferred_lft forever`

// ifconfigOutput is macOS `ifconfig`, the host this defect was found on.
const ifconfigOutput = `lo0: flags=8049<UP,LOOPBACK,RUNNING,MULTICAST> mtu 16384
	inet 127.0.0.1 netmask 0xff000000
	inet6 ::1 prefixlen 128
en0: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	inet6 fe80::10c%en0 prefixlen 64 secured scopeid 0xc
	inet 192.168.1.5 netmask 0xffffff00 broadcast 192.168.1.255
utun4: flags=8051<UP,POINTOPOINT,RUNNING,MULTICAST> mtu 1280
	inet 100.83.4.19 --> 100.83.4.19 netmask 0xff000000
bridge100: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	inet 192.168.64.1 netmask 0xffffff00 broadcast 192.168.64.255`

// namelessRunnerBundle is runnerBundle with its name taken away: a
// nameless bundle (INIT-005) that carries a colocated runner, which is the
// only combination the address check has an opinion about.
func namelessRunnerBundle(t *testing.T) *bundle.Bundle {
	t.Helper()
	b := runnerBundle(t)
	b.Manifest.Domain = ""
	b.Manifest.ACME = bundle.ACMEConfig{}
	if err := b.Manifest.Validate(); err != nil {
		t.Fatalf("nameless runner manifest is not valid: %v", err)
	}
	return b
}

// addressOptions is namelessOptions served at a specific address.
func addressOptions(remoteDir, address string) Options {
	opts := namelessOptions(remoteDir)
	opts.Address = address
	return opts
}

// The defect, end to end: a loopback address on a deployment that carries
// CI is refused before anything reaches the host, and the refusal names an
// address that host actually answers on rather than telling the operator to
// think of one.
func TestUpRefusesALoopbackAddressWhenItDeploysARunner(t *testing.T) {
	host := newFakeHost()
	host.hostAddrOutput = ipAddrOutput

	err := Up(context.Background(), events.NewJob(), host, namelessRunnerBundle(t), addressOptions("/opt/farrier", "127.0.0.1"))
	if err == nil {
		t.Fatal("Up: want a refusal for a loopback address with a colocated runner, got nil")
	}
	for _, want := range []string{"127.0.0.1", "loopback", "CI", "192.168.1.5", "colocatedRunner"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v\nwant it to mention %q", err, want)
		}
	}
	// The Docker bridge and loopback itself are addresses, and neither is
	// an answer to "where do other machines reach this host".
	for _, unwanted := range []string{"172.17.0.1", "fe80:"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("error = %v\nwant it not to suggest %q", err, unwanted)
		}
	}

	if len(host.files) != 0 {
		t.Errorf("Up wrote to the host before refusing: %v", hostPaths(host))
	}
	for _, command := range host.commands {
		if strings.Contains(command, "docker compose up") {
			t.Error("Up converged the host before refusing")
		}
	}
}

// The refusal is about CI, so it applies only where CI is. A browse-only
// instance on the operator's own machine has no container that has to reach
// it, and `-address 127.0.0.1` stays a legitimate way to run one (UP-006).
func TestUpAllowsALoopbackAddressWithoutAColocatedRunner(t *testing.T) {
	host := newFakeHost()
	host.hostAddrOutput = ipAddrOutput

	b := namelessRunnerBundle(t)
	off := false
	b.Manifest.Actions.ColocatedRunner = &off

	if err := Up(context.Background(), events.NewJob(), host, b, addressOptions("/opt/farrier", "127.0.0.1")); err != nil {
		t.Fatalf("Up: %v", err)
	}
	appINI := host.files["/opt/farrier/"+hostConfigDir+"/"+appINIFilename]
	if !strings.Contains(appINI, "ROOT_URL = http://127.0.0.1:") {
		t.Errorf("app.ini does not serve the address it was given:\n%s", appINI)
	}
}

// A loopback address is three spellings, not one, and pattern-matching the
// familiar one would leave the other two producing the same dead instance.
func TestUpRefusesEveryLoopbackSpelling(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "127.0.0.53", "::1", "[::1]", "localhost", "LocalHost", "db.localhost"} {
		host := newFakeHost()
		host.hostAddrOutput = ifconfigOutput

		err := Up(context.Background(), events.NewJob(), host, namelessRunnerBundle(t), addressOptions("/opt/farrier", address))
		if err == nil {
			t.Errorf("Up(-address %s): want a refusal, got nil", address)
			continue
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("Up(-address %s) = %v, want it to say the address is loopback", address, err)
		}
		if len(host.files) != 0 {
			t.Errorf("Up(-address %s) wrote to the host before refusing: %v", address, hostPaths(host))
		}
	}
}

// A host that cannot be asked for its own address still gets the
// deployment refused — the instance would be just as broken — and the
// operator still gets a next step (XCUT-003).
func TestUpRefusesALoopbackAddressWhenTheHostNamesNoAddress(t *testing.T) {
	for name, host := range map[string]*fakeHost{
		"discovery failed": func() *fakeHost { h := newFakeHost(); h.hostAddrErr = errAddressDiscovery; return h }(),
		"nothing but loopback": func() *fakeHost {
			h := newFakeHost()
			h.hostAddrOutput = "1: lo    inet 127.0.0.1/8 scope host lo"
			return h
		}(),
	} {
		err := Up(context.Background(), events.NewJob(), host, namelessRunnerBundle(t), addressOptions("/opt/farrier", "127.0.0.1"))
		if err == nil {
			t.Errorf("%s: want a refusal, got nil", name)
			continue
		}
		for _, want := range []string{"loopback", "reach this host at", "colocatedRunner"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error = %v\nwant it to mention %q", name, err, want)
			}
		}
		if len(host.files) != 0 {
			t.Errorf("%s: Up wrote to the host before refusing: %v", name, hostPaths(host))
		}
	}
}

// A routable address deploys, and the runner is pointed at it: the address
// is what the runner registers against and what the forge hands job
// containers to clone from, so those two being the same URL is the whole
// point of the refusal above.
func TestUpDeploysARoutableAddressWithItsRunner(t *testing.T) {
	host := newFakeHost()
	host.hostAddrOutput = ipAddrOutput

	if err := Up(context.Background(), events.NewJob(), host, namelessRunnerBundle(t), addressOptions("/opt/farrier", "192.168.1.5")); err != nil {
		t.Fatalf("Up: %v", err)
	}
	svc, ok := convergedCompose(t, host, "/opt/farrier")[forge.RunnerService].(map[string]any)
	if !ok {
		t.Fatalf("converged compose declares no %q service", forge.RunnerService)
	}
	if command := fmt.Sprint(svc["command"]); !strings.Contains(command, "http://192.168.1.5:") {
		t.Errorf("runner is not registered against the address the forge is served at:\n%s", command)
	}
	appINI := host.files["/opt/farrier/"+hostConfigDir+"/"+appINIFilename]
	if !strings.Contains(appINI, "ROOT_URL = http://192.168.1.5:") {
		t.Errorf("app.ini does not serve the address it was given:\n%s", appINI)
	}
}

// A named bundle carries no address at all — its domain is what every URL
// is built from — so the check has nothing to say about one.
func TestLoopbackCheckIgnoresANamedBundle(t *testing.T) {
	host := newFakeHost()
	host.hostAddrOutput = ipAddrOutput

	if err := Up(context.Background(), events.NewJob(), host, runnerBundle(t), testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}
}

func TestIsLoopbackAddress(t *testing.T) {
	loopback := []string{"127.0.0.1", "127.1.2.3", "::1", "[::1]", "localhost", "LOCALHOST", "api.localhost"}
	routable := []string{"192.168.1.5", "10.0.0.7", "100.83.4.19", "[fd00::5]", "fd00::5", "box.tail1234.ts.net", "localhostess.example.com"}

	for _, address := range loopback {
		if !isLoopbackAddress(address) {
			t.Errorf("isLoopbackAddress(%q) = false, want true", address)
		}
	}
	for _, address := range routable {
		if isLoopbackAddress(address) {
			t.Errorf("isLoopbackAddress(%q) = true, want false", address)
		}
	}
}

func TestParseHostAddressesReadsBothTools(t *testing.T) {
	cases := map[string]struct {
		out  string
		want []string
	}{
		"ip -o addr show":     {ipAddrOutput, []string{"192.168.1.5", "[fd00::5]"}},
		"ifconfig":            {ifconfigOutput, []string{"192.168.1.5", "100.83.4.19"}},
		"nothing to say":      {"", nil},
		"neither tool exists": {"sh: ip: not found", nil},
	}
	for name, tc := range cases {
		got := parseHostAddresses(tc.out)
		if len(got) != len(tc.want) {
			t.Errorf("%s: parseHostAddresses = %v, want %v", name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: parseHostAddresses = %v, want %v", name, got, tc.want)
				break
			}
		}
	}
}
