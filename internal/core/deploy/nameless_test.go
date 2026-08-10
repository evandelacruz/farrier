package deploy

import (
	"context"
	"fmt"
	"path"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/caddy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
)

// The tests here are UP-006's acceptance bar, driven end to end through Up
// against the fake host: a nameless bundle (INIT-005) is served over plain
// HTTP at an address the operator supplies at `up`, git over SSH is
// identical to what a named bundle gets, and the event stream both
// frontends read says the web UI is unencrypted.

const namelessAddress = "box.tail1234.ts.net"

// namelessOptions is testOptions with the operator's address supplied. The
// fake cert issuer stays wired in deliberately: a nameless deployment must
// not reach it, and a test that removed it could not tell the difference
// between "never called" and "not there to call".
func namelessOptions(remoteDir string) Options {
	opts := testOptions(remoteDir)
	opts.Address = namelessAddress
	return opts
}

// shippedCaddyfile returns what Up wrote to the host as Caddy's config.
func shippedCaddyfile(t *testing.T, host *fakeHost, remoteDir string) string {
	t.Helper()
	path := remoteDir + "/" + caddyConfigDir + "/" + caddyfileFilename
	file, ok := host.files[path]
	if !ok {
		t.Fatalf("no Caddyfile shipped to %s, wrote: %v", path, keysOf(host.files))
	}
	return file
}

// caddyPorts returns the ports the converged Compose definition publishes
// on the caddy service — forgePorts' counterpart for the terminator.
func caddyPorts(t *testing.T, host *fakeHost, remoteDir string) []string {
	t.Helper()
	svc, ok := convergedCompose(t, host, remoteDir)[caddy.Service].(map[string]any)
	if !ok {
		t.Fatalf("converged compose declares no %q service", caddy.Service)
	}
	raw, _ := svc["ports"].([]any)
	ports := make([]string, 0, len(raw))
	for _, p := range raw {
		ports = append(ports, fmt.Sprint(p))
	}
	return ports
}

// UP-006's first half: the address the operator supplies at `up` — not
// anything the bundle carries — becomes what Caddy answers for and what
// Forgejo tells browsers it is.
func TestUpServesANamelessBundleOverPlainHTTPAtTheSuppliedAddress(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()

	if err := Up(context.Background(), job, host, namelessBundle(t), namelessOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	caddyfile := shippedCaddyfile(t, host, "/opt/farrier")
	if !strings.Contains(caddyfile, "http://"+namelessAddress+" {") {
		t.Errorf("Caddyfile does not serve http://%s:\n%s", namelessAddress, caddyfile)
	}
	if strings.Contains(caddyfile, "tls ") {
		t.Errorf("Caddyfile carries a tls directive on a nameless deployment:\n%s", caddyfile)
	}

	appINI := host.files["/opt/farrier/"+hostConfigDir+"/"+appINIFilename]
	for _, want := range []string{
		"ROOT_URL = http://" + namelessAddress + "/",
		"DOMAIN = " + namelessAddress,
		"SSH_DOMAIN = " + namelessAddress,
	} {
		if !strings.Contains(appINI, want) {
			t.Errorf("app.ini missing %q:\n%s", want, appINI)
		}
	}

	if got := caddyPorts(t, host, "/opt/farrier"); len(got) != 1 || got[0] != plainHTTPPort+":"+plainHTTPPort {
		t.Errorf("caddy ports = %v, want [%s:%s] — a nameless instance publishes plain HTTP and nothing else", got, plainHTTPPort, plainHTTPPort)
	}
}

// A nameless bundle proved no zone and holds no TLS key material
// (INIT-005), so nothing about deploying one may reach an ACME server or
// ship a certificate. This is the check that keeps the two paths honestly
// separate rather than one path with an empty certificate.
func TestUpOnANamelessBundleIssuesNoCertificate(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()
	opts := namelessOptions("/opt/farrier")
	issuer := &fakeCertIssuer{}
	opts.CertIssuer = issuer

	if err := Up(context.Background(), job, host, namelessBundle(t), opts); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if len(issuer.calls) != 0 || len(issuer.existingSeen) != 0 {
		t.Errorf("certificate issuer was consulted %d times on a nameless deployment", len(issuer.existingSeen))
	}
	for _, name := range []string{certFilename, keyFilename} {
		if _, ok := host.files["/opt/farrier/"+caddyConfigDir+"/"+name]; ok {
			t.Errorf("nameless deployment shipped %s to the host", name)
		}
	}
}

// UP-006's second half: "with git over SSH unchanged". SSH encrypts on its
// own, so the one thing a nameless instance must not lose is the path that
// is already safe across the internet. Everything about it — the published
// port mapping, the container-side listener, the bundle's own host key — is
// asserted to be what a named deployment produces.
func TestUpOnANamelessBundleLeavesGitOverSSHUnchanged(t *testing.T) {
	namelessHost := newFakeHost()
	if err := Up(context.Background(), events.NewJob(), namelessHost, namelessBundle(t), namelessOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up (nameless): %v", err)
	}
	namedHost := newFakeHost()
	if err := Up(context.Background(), events.NewJob(), namedHost, testBundle(t), testOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up (named): %v", err)
	}

	gotPorts := forgePorts(t, namelessHost, "/opt/farrier")
	wantPorts := forgePorts(t, namedHost, "/opt/farrier")
	if strings.Join(gotPorts, ",") != strings.Join(wantPorts, ",") {
		t.Errorf("nameless forgejo ports = %v, named = %v — git over SSH must be published identically", gotPorts, wantPorts)
	}

	hostKeyPath := path.Join(GiteaStatePath("/opt/farrier"), sshHostKeyRelPath())
	if namelessHost.files[hostKeyPath] == "" {
		t.Errorf("nameless deployment installed no ssh host key at %s", hostKeyPath)
	}
	if namelessHost.files[hostKeyPath] != namedHost.files[hostKeyPath] {
		t.Error("nameless deployment installed a different ssh host key than a named one from the same bundle")
	}

	appINI := namelessHost.files["/opt/farrier/"+hostConfigDir+"/"+appINIFilename]
	namedINI := namedHost.files["/opt/farrier/"+hostConfigDir+"/"+appINIFilename]
	for _, line := range []string{
		"SSH_PORT = ",
		"SSH_LISTEN_PORT = ",
		"START_SSH_SERVER = ",
		"SSH_SERVER_HOST_KEYS = ",
	} {
		if got, want := lineWithPrefix(appINI, line), lineWithPrefix(namedINI, line); got != want {
			t.Errorf("nameless app.ini %q, named %q — the SSH section must be identical", got, want)
		}
	}
}

// The clone URL reported for a nameless instance is addressed at the
// operator's address rather than at an empty domain, and is spelled by the
// same manifest function `publish` writes into `origin` (IMPT-004).
func TestUpReportsTheNamelessCloneURLAtTheSuppliedAddress(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()

	if err := Up(context.Background(), job, host, namelessBundle(t), namelessOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	detail := stepDetail(t, drain(job), StepConfigureGitSSH)
	want := (&bundle.Manifest{}).GitSSHCloneURLAt(namelessAddress, "<owner>", "<repo>")
	if !strings.Contains(detail, want) {
		t.Errorf("git-ssh detail = %q, want it to carry the clone URL %q", detail, want)
	}
}

// UP-006's third part, and the one the operator's safety rests on: the
// unencrypted web UI is stated through the CORE-002 event stream, so the
// CLI and the dashboard both carry it, and it says what to do about it.
func TestUpStatesTheWebUIIsUnencryptedThroughTheEventStream(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()

	if err := Up(context.Background(), job, host, namelessBundle(t), namelessOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}
	evs := drain(job)

	detail := stepDetail(t, evs, StepConfigureHTTP)
	for _, want := range []string{"unencrypted", "trusted network", "UP-006"} {
		if !strings.Contains(detail, want) {
			t.Errorf("configure-http detail = %q, want it to mention %q", detail, want)
		}
	}
	if !strings.Contains(detail, "Git over SSH is encrypted") {
		t.Errorf("configure-http detail = %q, want it to say git over SSH stays encrypted", detail)
	}

	// The last thing Up says is the URL to open, and for a nameless
	// instance the caveat rides along with it.
	ready := stepDetail(t, evs, StepWaitCaddy)
	if !strings.Contains(ready, "http://"+namelessAddress+"/") || strings.Contains(ready, "https://") {
		t.Errorf("wait-caddy detail = %q, want the plain-HTTP endpoint", ready)
	}
	if !strings.Contains(ready, "unencrypted") {
		t.Errorf("wait-caddy detail = %q, want the unencrypted caveat beside the URL", ready)
	}

	// No step claims to have configured TLS on a deployment that
	// configured none.
	for _, ev := range evs {
		if ev.Step == StepConfigureTLS {
			t.Errorf("nameless deployment emitted a %s event: %+v", StepConfigureTLS, ev)
		}
	}
}

// CI on a nameless instance reaches it at the same URL the operator's
// browser does — the runner cannot resolve a domain the bundle does not
// have (forge.InstanceURL).
func TestUpPointsTheRunnerAtTheNamelessInstanceURL(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()

	b := runnerBundle(t)
	b.Manifest.Domain = ""
	b.Manifest.ACME = bundle.ACMEConfig{}
	if err := b.Manifest.Validate(); err != nil {
		t.Fatalf("nameless runner manifest is not valid: %v", err)
	}
	if err := Up(context.Background(), job, host, b, namelessOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up: %v", err)
	}

	svc, ok := convergedCompose(t, host, "/opt/farrier")[forge.RunnerService].(map[string]any)
	if !ok {
		t.Fatalf("converged compose declares no %q service", forge.RunnerService)
	}
	command := fmt.Sprint(svc["command"])
	if !strings.Contains(command, "http://"+namelessAddress+"/") {
		t.Errorf("runner command is not pointed at http://%s/:\n%s", namelessAddress, command)
	}
	if strings.Contains(command, "https://") {
		t.Errorf("runner command reaches for HTTPS on a nameless instance:\n%s", command)
	}
}

// A named bundle already answers "where is this reached" with its domain,
// and `up` completes with HTTPS at it (UP-002). A second, conflicting
// answer is a mistake worth failing on rather than ignoring.
func TestUpRejectsAnAddressForANamedBundle(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()

	err := Up(context.Background(), job, host, testBundle(t), func() Options {
		opts := testOptions("/opt/farrier")
		opts.Address = namelessAddress
		return opts
	}())
	if err == nil {
		t.Fatal("Up: want error for an address on a named bundle, got nil")
	}
	if !strings.Contains(err.Error(), "UP-002") {
		t.Errorf("error = %v, want it to name the HTTPS guarantee it conflicts with", err)
	}
	if len(host.files) != 0 || len(host.commands) != 0 {
		t.Error("Up touched the host before refusing")
	}
}

// DRIL-002's containment is defined against a named instance (the domain on
// loopback, aliased onto Caddy). What it means for a nameless one is not
// settled, and guessing would risk a drilled runner reaching production —
// so the combination is refused rather than approximated.
func TestUpRejectsAQuarantinedNamelessDeployment(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()

	opts := namelessOptions("/opt/farrier")
	opts.Quarantine = true
	err := Up(context.Background(), job, host, namelessBundle(t), opts)
	if err == nil {
		t.Fatal("Up: want error for a quarantined nameless deployment, got nil")
	}
	if !strings.Contains(err.Error(), "DRIL-002") {
		t.Errorf("error = %v, want it to name DRIL-002", err)
	}
	if len(host.files) != 0 || len(host.commands) != 0 {
		t.Error("Up touched the host before refusing")
	}
}

// Re-running `up` at the same address converges rather than diverging
// (UP-003), and the plain-HTTP config is written where the named path
// writes its own, so an instance can only ever have one Caddy config.
func TestUpOnANamelessBundleIsRepeatable(t *testing.T) {
	first := newFakeHost()
	if err := Up(context.Background(), events.NewJob(), first, namelessBundle(t), namelessOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up (first): %v", err)
	}
	second := newFakeHost()
	if err := Up(context.Background(), events.NewJob(), second, namelessBundle(t), namelessOptions("/opt/farrier")); err != nil {
		t.Fatalf("Up (second): %v", err)
	}

	if got, want := shippedCaddyfile(t, second, "/opt/farrier"), shippedCaddyfile(t, first, "/opt/farrier"); got != want {
		t.Errorf("second run shipped a different Caddyfile:\n%s\nwant\n%s", got, want)
	}
}

func TestServeAddressNormalizesWhatItAccepts(t *testing.T) {
	cases := map[string]string{
		"192.168.1.5":         "192.168.1.5",
		"  box.local  ":       "box.local",
		"box.tail1234.ts.net": "box.tail1234.ts.net",
		"fd00::1":             "[fd00::1]",
		"[fd00::1]":           "[fd00::1]",
		"forge":               "forge",
	}
	for in, want := range cases {
		got, err := serveAddress(&bundle.Manifest{}, in)
		if err != nil {
			t.Errorf("serveAddress(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("serveAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

// Everything rejected here would otherwise land verbatim in a ROOT_URL, a
// Caddy site address, and a clone URL, producing an instance that comes up
// and cannot be used.
func TestServeAddressRejectsWhatIsNotAnIPOrHostname(t *testing.T) {
	cases := map[string]string{
		"scheme":         "http://box.local",
		"port":           "box.local:8080",
		"path":           "box.local/forge",
		"user":           "git@box.local",
		"query":          "box.local?x=1",
		"space":          "box local",
		"empty label":    "box..local",
		"trailing dot":   "box.local.",
		"leading hyphen": "-box.local",
		"underscore":     "box_local",
		"bad v6":         "[not-an-address]",
	}
	for name, in := range cases {
		if got, err := serveAddress(&bundle.Manifest{}, in); err == nil {
			t.Errorf("serveAddress(%s = %q) = %q, want error", name, in, got)
		}
	}
}

// lineWithPrefix returns the first line of ini starting with prefix, or ""
// — enough to compare one rendered key between two app.ini files.
func lineWithPrefix(ini, prefix string) string {
	for _, line := range strings.Split(ini, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}
