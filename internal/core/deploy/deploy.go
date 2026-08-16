// Package deploy implements UP-001: deploying the full stateless layer
// (spec.md "Stateless vs. stateful" — the forge app, CI orchestration, and
// runners) to a target host given only an ssh://user@host address and a
// bundle. It also implements UP-002: ending that deployment with the forge
// serving HTTPS at the bundle domain — a guarantee about a *named* bundle,
// see "Named bundles only" below; UP-003: re-running Up against a host
// it has already deployed to is safe and converges that host to the
// bundle definition, rather than requiring a fresh host every time; and
// UP-004: forge state — git repositories and the database — lives on the
// host under RemoteDir/state, bind-mounted into the container that serves
// it, so recreating that container never destroys it; UP-005: git over
// SSH is served at the bundle domain on the host port the manifest
// declares, using the bundle's own SSH host key; and UP-008: a deployment
// onto host state that belongs to a different instance is refused before
// anything on that host is touched (stateowner.go).
//
// # Named and nameless
//
// Everything this package deploys is addressed by one host: the Caddy site
// it renders, the ROOT_URL in app.ini, the clone URLs Forgejo displays. A
// named bundle carries that host — its domain — and gets the certificate
// Up issues and HTTPS at it (UP-002). A nameless bundle (INIT-005) carries
// none, so `up` is where the instance learns how it is reached: the
// operator supplies an IP or a hostname, Caddy terminates in plain HTTP at
// it, and no certificate exists anywhere in the deployment (UP-006).
//
// The two differ in exactly two places — which host every URL is built
// from, and whether Caddy is handed a certificate — and Up branches once,
// at the step that configures the terminator. Everything else, git over
// SSH above all, is identical: SSH encrypts on its own, so a nameless
// instance is safe to push to across the internet even though its web UI
// is not safe to log in to across one. That asymmetry is the whole of the
// trade spec.md "Instances without a name" records, and Up states it
// plainly through the event stream rather than leaving the operator to
// notice a missing padlock (plainHTTPDetail).
//
// A bundle and an address that disagree — an address for a named bundle,
// or none for a nameless one — is refused before Up touches the host
// (serveAddress).
//
// It is the sequencing layer over packages that already do the real work:
// orchestrate (SSH transport, Compose rendering and convergence, ORCH-001
// and ORCH-002), forge (app.ini rendering and admin bootstrap, FORGE-001
// and FORGE-002), caddy (Caddyfile rendering), and acme (certificate
// issuance, ACME-001). Up is the first thing that calls them together, in
// the order a real deployment needs.
package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/caddy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// Step identifiers Up emits through the job's event stream (CORE-002), in
// the order it runs them. forge.StepAdminBootstrap follows StepWaitForge,
// and forge.StepRunnerRegister follows that.
//
// StepWaitForge is what makes that order mean anything: it does not
// complete until Forgejo can actually open its database (waitForge), so the
// admin account the next step creates lands in a schema that exists.
//
// StepConfigureTLS and StepConfigureHTTP are alternatives, not a sequence:
// a deployment runs exactly one of them, whichever the bundle calls for
// (UP-002 for a named bundle, UP-006 for a nameless one). They are separate
// identifiers rather than one step with two details so that a frontend
// rendering the stream shows what actually happened — a step named
// "configure-tls" on a deployment that configured no TLS would be a lie
// told by a progress bar.
const (
	StepCheckHost       = "check-host"
	StepCheckOwner      = "check-state-owner"
	StepConfigureForge  = "configure-forge"
	StepConfigureTLS    = "configure-tls"
	StepConfigureHTTP   = "configure-http"
	StepConfigureState  = "configure-state"
	StepConfigureGitSSH = "configure-git-ssh"
	StepConfigureRunner = "configure-runner"
	StepCheckVersion    = "check-state-version"
	StepConverge        = "converge"
	StepWaitForge       = "wait-forge"
	StepWaitCaddy       = "wait-caddy"
)

// hostConfigDir is the directory under RemoteDir Up writes deploy-time
// forge configuration to. It is deploy-time content, not bundle content
// (KEY-003; see forge.RenderAppINI's doc comment), so it lives alongside
// the shipped Compose files rather than inside the bundle directory itself.
const hostConfigDir = "forge"

// appINIFilename is the file Up ships app.ini under, inside hostConfigDir.
const appINIFilename = "app.ini"

// appINIChecksumEnv is the environment variable Up sets on the forgejo
// service to a checksum of the app.ini it just shipped (see WithEnv's doc
// comment for why: a bind-mounted file's content isn't otherwise part of
// what `docker compose up -d` decides to recreate on).
const appINIChecksumEnv = "FARRIER_APP_INI_CHECKSUM"

// readyTimeout bounds how long Up waits for a container that only has to be
// running — caddy — to accept `docker compose exec` after Converge starts it.
const readyTimeout = 60 * time.Second

// forgeReadyTimeout bounds how long Up waits for Forgejo, which has to be
// more than running (waitForge). It is deliberately several times
// readyTimeout: on a host whose state directory is fresh, the wait covers
// Forgejo's entire first-boot migration set, and that runs on whatever
// machine the operator brought — a cold laptop with Docker itself still
// warming up is the ordinary case, not the pathological one. The budget is
// what a stuck deployment costs, and waiting three minutes to be told the
// truth beats being told a lie in one.
//
// A var, not a const, for the same reason readyInterval is: the test that
// covers a Forgejo which never finishes would otherwise take three minutes
// to run.
var forgeReadyTimeout = 180 * time.Second

// readyInterval is the delay between readiness probes within a wait's
// budget. A var, not a const, so tests can shorten it rather than actually
// waiting.
var readyInterval = 2 * time.Second

// logTailLines is how many lines of the forgejo container's log a wait that
// ran out of budget reports back (waitForge).
const logTailLines = 50

// Host is everything Up needs from a connected SSH session: orchestrate.
// Transport (so Up can hand it straight to orchestrate.Converge) plus Run
// and CheckHost. *orchestrate.Client (ORCH-001) satisfies it in
// production; tests use a fake, so Up's sequencing can be asserted without
// a real SSH server. Up does not connect host or call its Close — that is
// the caller's job, symmetric with orchestrate.Converge taking an
// already-reachable Transport rather than connecting one itself.
type Host interface {
	orchestrate.Transport
	Run(ctx context.Context, command string, stdout, stderr io.Writer) error
	CheckHost(ctx context.Context) error
}

// Options configures Up beyond the target host and bundle.
type Options struct {
	// RemoteDir is the directory on the host Up deploys into: Compose
	// files, and the rendered forge and Caddy config, live under it.
	// Required.
	RemoteDir string

	// Address is the address — an IP or a hostname — the operator serves a
	// nameless bundle's web UI at (UP-006). Required for a nameless bundle
	// and rejected for a named one, which already answers the same question
	// with its domain; see serveAddress for why the two exclude each other.
	//
	// It is a deploy-time parameter rather than a manifest field on
	// purpose. A nameless bundle is the one case spec.md "Instances without
	// a name" lets opt out of identity-in-the-bundle, so the address is not
	// bundle identity to carry through backup, restore, and promote — it is
	// how *this* deployment is reached, and a restored or promoted instance
	// on a different host is reached at a different one. Attaching a real
	// name later is UP-007's in-place operation, and that one does write
	// the manifest.
	Address string

	// CertIssuer resolves the TLS certificate Up hands to Caddy (UP-002),
	// reusing the one init persisted unless it's due for renewal (UP-003).
	// Nil uses the real ACME-backed issuer (acme.EnsureValid); tests
	// substitute a fake so Up's sequencing is assertable without a real
	// ACME server.
	CertIssuer CertIssuer

	// Migrate declares that this deployment is deliberately starting a
	// different Forgejo version against the host's existing state, and so
	// may run schema migrations. Only internal/core/upgrade sets it — that
	// is the whole of UPGR-003's "during `upgrade` and at no other time",
	// and it is why `up` has no flag for it. See checkStateVersion.
	Migrate bool

	// Quarantine declares that this deployment is a rehearsal rather than a
	// running instance, and must not be able to reach the outside world or
	// be reached from it (DRIL-002). It changes two things and nothing
	// else: app.ini is rendered with outbound webhooks, email, and mirrors
	// disabled (forge.AppINIOptions.Quarantine), and Caddy's HTTPS port is
	// published on the host's loopback interface instead of every
	// interface, so the instance is reachable through an SSH tunnel and
	// from nowhere else (configureTLS).
	//
	// Only internal/core/drill sets it — a drill instance carries
	// production's identity (spec.md "Rehearsal") — and it sets it on every
	// drill, not only when a smoke job happens to be running. `up` has no
	// flag for it for the same reason `up` has no Migrate flag: a real
	// deployment that cannot be reached is not a deployment.
	Quarantine bool
}

// Up deploys b's full stateless layer to host (UP-001): it verifies Docker
// is reachable, resolves the bundle's key material and renders and ships
// Forgejo's app.ini, resolves the bundle's persisted TLS certificate and
// renders and ships Caddy's config (UP-002), gives forge state a durable
// home on the host and bind-mounts it into the forgejo service (UP-004),
// installs the bundle's persisted ed25519 SSH host key where that app.ini
// points Forgejo's git-over-SSH server at (RSTR-004) and publishes that
// server on the host port the manifest declares so `git clone` and `git
// push` over SSH work against a fresh deployment (UP-005), wires up the colocated
// Actions runner unless the bundle turns it off (FORGE-005), converges the
// host to the bundle's Compose definition plus that config, waits for
// Forgejo to finish setting up its database (waitForge — the container
// accepting a command is not the same thing, and on a fresh host it is true
// first), provisions the first admin account, registers
// that runner against the instance, and waits for Caddy to accept commands
// so the forge is serving HTTPS and usable in a browser before Up returns
// (UP-002).
//
// A nameless bundle (INIT-005) takes the same path with one substitution:
// opts.Address, the address the operator supplies at `up`, stands in for
// the domain the bundle does not have, Caddy is configured to terminate in
// plain HTTP at it rather than to serve a certificate, and Up completes
// with the forge serving HTTP there instead of HTTPS (UP-006). Git over SSH
// is byte-for-byte what a named bundle gets. See "Named and nameless" in
// the package doc for what that trades and why the two are one path.
//
// Every step is safe to repeat against a host Up has already deployed to
// (UP-003): CheckHost and the readiness waits are read-only — the forge
// wait's second probe lists Forgejo's users and changes nothing
// (forge.ReadyCommand) — configureForge always
// re-ships app.ini and re-derives its checksum so a changed manifest is
// visible to Converge (WithEnv's doc comment), configureTLS reuses the
// persisted certificate untouched unless it's actually due for renewal
// (configureTLS's doc comment), configureState's directory creation, chown,
// and access probe all leave an already-correct host as they found it
// (configureState's doc comment),
// configureSSHHostKey always writes the same persisted key back (its own
// doc comment), publishGitSSH derives its port mapping from the manifest
// alone, orchestrate.Converge is idempotent by construction (its
// own doc comment), forge.Bootstrap treats an admin account that already
// exists as done rather than a failure, configureRunner writes the same
// non-rotating secret back and derives its mounts from the manifest alone
// (its own doc comment), and runner registration is an upsert keyed by that
// secret, so a re-run updates the existing registration rather than creating
// a second one (forge.RegisterRunner), and checkStateOwner writes the same
// claim back over state that is already this instance's.
//
// Two steps do read the host back before deciding. checkStateOwner refuses
// to deploy onto forge state that belongs to a different instance, naming
// what it found and leaving the host untouched (UP-008) — it runs first, so
// nothing is written to a host this deployment turns out not to own.
// checkStateVersion refuses to start a Forgejo version other than the one
// the host's state was last started under, because that is what makes
// Forgejo migrate its schema, and UPGR-003 puts migrations under `upgrade`
// alone. Up otherwise lets the bundle alone determine the outcome, same as
// Converge.
//
// Up owns job's terminal event: it calls job.Succeeded or job.Failed
// exactly once, after every step below has run (or the first one fails),
// and returns the same error it reported through job.
func Up(ctx context.Context, job *events.Job, host Host, b *bundle.Bundle, opts Options) error {
	if err := up(ctx, job, host, b, opts); err != nil {
		job.Failed(err.Error())
		return err
	}
	job.Succeeded("forge deployed")
	return nil
}

func up(ctx context.Context, job *events.Job, host Host, b *bundle.Bundle, opts Options) error {
	if b == nil {
		return fmt.Errorf("deploy: bundle is required")
	}
	if strings.TrimSpace(opts.RemoteDir) == "" {
		return fmt.Errorf("deploy: remote directory is required")
	}
	// Which of the two endpoints this deployment serves is decided here,
	// ahead of CheckHost, so a bundle and an address that disagree leave
	// the host exactly as Up found it rather than failing partway through
	// configureForge with the same news. address is empty for a named
	// bundle and set for a nameless one, and every step below reads
	// Manifest.Named() to tell which it is holding.
	address, err := serveAddress(&b.Manifest, opts.Address)
	if err != nil {
		return err
	}
	// Checked here, beside the address, and for the same reason: a bundle
	// that publishes its web port somewhere other than the standard one
	// without saying where clients connect would deploy a forge whose every
	// rendered URL is wrong (bundle.Manifest.ValidateWebPorts). Refusing
	// before CheckHost leaves the host exactly as Up found it.
	if err := b.Manifest.ValidateWebPorts(); err != nil {
		return err
	}
	// DRIL-002's containment is defined against a named instance: the
	// drilled instance answers at the bundle domain on loopback, and Caddy
	// carries that domain as a network alias so the drilled runner reaches
	// the drilled instance rather than production. A nameless instance has
	// no domain to alias and its address may be a bare IP, so what
	// containment means for one is an open question rather than a detail
	// — and a drill that got it wrong would point a second runner at
	// production's job queue. Refuse the combination instead of guessing.
	if opts.Quarantine && !b.Manifest.Named() {
		return errors.New("deploy: a quarantined deployment of a nameless bundle is not supported; drill a named bundle, or attach a name first")
	}

	job.Started(StepCheckHost, "checking Docker is reachable")
	if err := host.CheckHost(ctx); err != nil {
		job.Emit(StepCheckHost, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: check host: %w", err)
	}
	job.Emit(StepCheckHost, events.StateSucceeded, "Docker reachable")

	// Before the first byte this deployment writes to the host: whose forge
	// state is already in that directory (UP-008). A deployment onto another
	// instance's state is refused here, with the host exactly as Up found it.
	job.Started(StepCheckOwner, "checking whose forge state is in the deployment directory")
	ownerDetail, err := checkStateOwner(ctx, host, b, opts.RemoteDir)
	if err != nil {
		job.Emit(StepCheckOwner, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: %w", err)
	}
	job.Emit(StepCheckOwner, events.StateSucceeded, ownerDetail)

	job.Started(StepConfigureForge, "resolving key material and rendering app.ini")
	compose, secrets, err := configureForge(ctx, host, b, opts.RemoteDir, address, opts.Quarantine)
	if err != nil {
		job.Emit(StepConfigureForge, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: configure forge: %w", err)
	}
	if opts.Quarantine {
		job.Emit(StepConfigureForge, events.StateSucceeded, "app.ini rendered and shipped with outbound webhooks, email, and mirrors disabled for quarantine")
	} else {
		job.Emit(StepConfigureForge, events.StateSucceeded, "app.ini rendered and shipped")
	}

	if b.Manifest.Named() {
		job.Started(StepConfigureTLS, "resolving persisted certificate and rendering caddy config")
		var renewed bool
		compose, renewed, err = configureTLS(ctx, host, b, opts.RemoteDir, compose, issuerOrDefault(opts.CertIssuer), opts.Quarantine)
		if err != nil {
			job.Emit(StepConfigureTLS, events.StateFailed, err.Error())
			return fmt.Errorf("deploy: configure tls: %w", err)
		}
		if renewed {
			job.Emit(StepConfigureTLS, events.StateSucceeded, "certificate was due for renewal; a fresh one was issued and persisted to the keystore")
		} else {
			job.Emit(StepConfigureTLS, events.StateSucceeded, "persisted certificate reused and caddy configured")
		}
	} else {
		job.Started(StepConfigureHTTP, fmt.Sprintf("rendering plain-HTTP caddy config for %s", address))
		compose, err = configurePlainHTTP(ctx, host, &b.Manifest, opts.RemoteDir, compose, address)
		if err != nil {
			job.Emit(StepConfigureHTTP, events.StateFailed, err.Error())
			return fmt.Errorf("deploy: configure plain http: %w", err)
		}
		job.Emit(StepConfigureHTTP, events.StateSucceeded, plainHTTPDetail(&b.Manifest, address))
	}

	job.Started(StepConfigureState, "creating host state directories")
	var stateOwned bool
	compose, stateOwned, err = configureState(ctx, host, b, opts.RemoteDir, compose)
	if err != nil {
		job.Emit(StepConfigureState, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: configure state: %w", err)
	}
	if stateOwned {
		job.Emit(StepConfigureState, events.StateSucceeded, "state directories ready and mounted")
	} else {
		job.Emit(StepConfigureState, events.StateSucceeded, "state directories ready and mounted; this host does not let ownership be set from here, and the forge can read and write them anyway")
	}

	job.Started(StepConfigureGitSSH, "installing ssh host key and publishing git over ssh")
	if err := configureSSHHostKey(ctx, host, b, opts.RemoteDir); err != nil {
		job.Emit(StepConfigureGitSSH, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: configure ssh host key: %w", err)
	}
	compose, err = publishGitSSH(compose, &b.Manifest, opts.Quarantine)
	if err != nil {
		job.Emit(StepConfigureGitSSH, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: configure git over ssh: %w", err)
	}
	job.Emit(StepConfigureGitSSH, events.StateSucceeded, gitSSHDetail(&b.Manifest, address, opts.Quarantine))

	job.Started(StepConfigureRunner, "configuring the colocated actions runner")
	compose, runnerDeployed, err := configureRunner(ctx, host, b, opts.RemoteDir, address, compose, opts.Quarantine)
	if err != nil {
		job.Emit(StepConfigureRunner, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: configure runner: %w", err)
	}
	if runnerDeployed {
		job.Emit(StepConfigureRunner, events.StateSucceeded, "colocated runner configured with the host's docker socket (spec.md \"CI trust boundary\")")
	} else {
		job.Emit(StepConfigureRunner, events.StateSucceeded, fmt.Sprintf("no colocated runner in this deployment; register a remote runner against %s to run CI", forge.InstanceURL(&b.Manifest, address)))
	}

	job.Started(StepCheckVersion, "checking the pinned forgejo version against the host's state")
	detail, err := checkStateVersion(ctx, host, b.Manifest.Images[forge.Service], opts.RemoteDir, opts.Migrate)
	if err != nil {
		job.Emit(StepCheckVersion, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: %w", err)
	}
	job.Emit(StepCheckVersion, events.StateSucceeded, detail)

	job.Started(StepConverge, "converging host to bundle definition")
	deployed := &bundle.Bundle{Manifest: b.Manifest, Compose: compose}
	if err := orchestrate.Converge(ctx, host, opts.RemoteDir, deployed); err != nil {
		job.Emit(StepConverge, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: converge: %w", err)
	}
	job.Emit(StepConverge, events.StateSucceeded, "host converged")

	runner := &composeRunner{host: host, remoteDir: opts.RemoteDir, bundle: deployed}

	job.Started(StepWaitForge, "waiting for forgejo to finish setting up its database")
	if err := waitForge(ctx, runner, secrets); err != nil {
		job.Emit(StepWaitForge, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: wait for forgejo: %w", err)
	}
	job.Emit(StepWaitForge, events.StateSucceeded, "forgejo is up and its database is ready")

	account, err := forge.NewAdminAccount(forge.AdminEmailDomain(&b.Manifest))
	if err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	if err := forge.Bootstrap(ctx, runner, job, account); err != nil {
		return fmt.Errorf("deploy: %w", err)
	}

	// Registration is deliberately after Converge rather than before it:
	// it needs a running Forgejo, and the runner container the same
	// Converge started retries its connection until this lands.
	if runnerDeployed {
		if err := forge.RegisterRunner(ctx, runner, job, RunnerSecretPath(opts.RemoteDir)); err != nil {
			return fmt.Errorf("deploy: %w", err)
		}
	}

	job.Started(StepWaitCaddy, "waiting for caddy to accept commands")
	// forge.Secrets{} rather than the deployment's: caddy is never handed
	// Forgejo's key material, so there is nothing here to scrub out of what
	// its probe says. Redact on a zero value leaves the text alone — it
	// replaces each field's value only when that field is set.
	if err := waitReady(ctx, runner, readyTimeout, forge.Secrets{}, probe{
		command:    execProbe(caddy.Service),
		waitingFor: "caddy did not become ready",
	}); err != nil {
		job.Emit(StepWaitCaddy, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: wait for caddy: %w", err)
	}
	job.Emit(StepWaitCaddy, events.StateSucceeded, caddyReadyDetail(&b.Manifest, address))
	return nil
}

// configureForge resolves the bundle's key material, renders app.ini,
// ships it to host, and returns b's Compose files with a bind mount added
// so the forgejo service loads the file that was just shipped, plus the key
// material it resolved.
//
// That last return is not for use — nothing downstream renders config
// again — but for scrubbing: waitForge reports the forgejo container's log
// when a deployment stalls, and the values Forgejo was configured with are
// what that report has to be scrubbed of (forge.Secrets.Redact, KEY-003).
//
// address is the operator-supplied address a nameless bundle is served at
// (UP-006) and is empty for a named one, which addresses itself; app.ini's
// ROOT_URL, DOMAIN, and SSH_DOMAIN all follow from whichever it is.
//
// quarantine renders the file with outbound webhooks, email, and mirrors
// disabled (DRIL-002). It changes only what is rendered here — the app.ini
// checksum below is derived from whatever was rendered, so a quarantined
// deployment and an ordinary one are distinguishable to Converge and a
// host converged one way is actually re-converged when converged the
// other.
func configureForge(ctx context.Context, host Host, b *bundle.Bundle, remoteDir, address string, quarantine bool) (map[string][]byte, forge.Secrets, error) {
	driver, err := keystore.New(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		return nil, forge.Secrets{}, fmt.Errorf("keystore driver: %w", err)
	}
	secrets, err := forge.ResolveSecrets(ctx, driver)
	if err != nil {
		return nil, forge.Secrets{}, fmt.Errorf("resolve key material: %w", err)
	}
	appINI, err := forge.RenderAppINI(&b.Manifest, secrets, forge.AppINIOptions{Quarantine: quarantine, Address: address})
	if err != nil {
		return nil, forge.Secrets{}, fmt.Errorf("render app.ini: %w", err)
	}

	hostPath := path.Join(remoteDir, hostConfigDir, appINIFilename)
	if err := host.WriteFile(ctx, hostPath, appINI, 0o600); err != nil {
		return nil, forge.Secrets{}, fmt.Errorf("ship app.ini: %w", err)
	}

	compose, err := orchestrate.WithBindMount(b.Compose, forge.Service, hostPath, forge.AppINIPath)
	if err != nil {
		return nil, forge.Secrets{}, fmt.Errorf("mount app.ini: %w", err)
	}

	sum := sha256.Sum256(appINI)
	compose, err = orchestrate.WithEnv(compose, forge.Service, appINIChecksumEnv, hex.EncodeToString(sum[:]))
	if err != nil {
		return nil, forge.Secrets{}, fmt.Errorf("set app.ini checksum: %w", err)
	}
	return compose, secrets, nil
}

// probe is one question a wait asks the host: command, run until it
// succeeds, and waitingFor — what the operator is told was never answered
// when the budget runs out. waitingFor is operator-facing prose and reads as
// the first half of a sentence the budget completes ("... within 3m0s").
type probe struct {
	command    string
	waitingFor string
}

// execProbe asks whether service accepts a `docker compose exec` at all.
// It is the first thing every wait asks, because nothing else can be asked
// until it is true — and it is all a service whose readiness is the same
// question as its container's, like caddy, has to answer.
func execProbe(service string) string {
	return fmt.Sprintf("docker compose exec -T %s true", service)
}

// waitReady polls each probe in order until it succeeds, or the shared
// budget elapses. Converge's `docker compose up -d` returns once containers
// are created and started, which says nothing about whether the software
// inside them has finished coming up; the steps after would then fail
// intermittently rather than deterministically, so this waits the race out
// instead of leaving it as a heisenbug in the caller.
//
// The budget covers the probes together rather than each in turn: they are
// phases of one wait for one service, and an operator who was told a
// deployment gets three minutes means three minutes in total.
//
// secrets is what a failed probe's output is scrubbed of before it reaches
// the caller. A probe runs software the deployment handed key material to —
// forge.ReadyCommand runs the same Forgejo binary against the same app.ini —
// and what that software echoes back on a config failure is its business,
// not something this has to be right about (KEY-003, forge.Secrets.Redact).
// A caller whose service was handed none passes the zero value, which
// redacts nothing.
func waitReady(ctx context.Context, runner forge.Runner, budget time.Duration, secrets forge.Secrets, probes ...probe) error {
	deadline := time.Now().Add(budget)
	for _, p := range probes {
		for {
			var stdout, stderr bytes.Buffer
			err := runner.Run(ctx, p.command, &stdout, &stderr)
			if err == nil {
				break
			}
			if !time.Now().Before(deadline) {
				return fmt.Errorf("%s within %s: %s", p.waitingFor, budget, secrets.Redact(probeFailure(err, &stdout, &stderr)))
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(readyInterval):
			}
		}
	}
	return nil
}

// probeFailure is what a probe that never succeeded left for the operator
// to read. Both streams are captured for the reason forge.FailureDetail
// documents — Forgejo's CLI does not commit to one, and a probe reading only
// one of them reports a failure with no message — and the transport's own
// error stands in when the command produced no output at all, which is what
// a dropped session or a container that is not there looks like.
//
// What it returns is unscrubbed by construction — it is someone else's
// output — so every caller redacts it before reporting it (KEY-003).
func probeFailure(err error, stdout, stderr *bytes.Buffer) string {
	if strings.TrimSpace(stdout.String()) == "" && strings.TrimSpace(stderr.String()) == "" {
		return err.Error()
	}
	return forge.FailureDetail(stdout, stderr)
}

// waitForge waits for Forgejo to be usable, which is not the same as its
// container being up.
//
// On a host whose state directory is fresh, Forgejo's first boot runs its
// entire migration set to create the database schema, and the container
// accepts an exec seconds before that finishes. A wait that stopped there
// would hand a schemaless database to forge.Bootstrap, which fails with
// "no such table: user" — and only on a host that has never booted before,
// which is every operator's first deployment and no re-run afterwards. So
// the container accepting an exec is the first phase, not the answer, and
// the second phase asks Forgejo itself (forge.ReadyCommand).
//
// A Forgejo that never comes up — an unwritable state directory, a bad
// image, a config it refuses — also ends here, and the reason is in its
// container log rather than in the probe's own output, so the failure
// carries the tail of that log. secrets is what both the log and the
// probes' own output are scrubbed of before either goes anywhere: Forgejo
// was handed key material in app.ini and what it echoes back on a config
// failure is its business, not something this should have to be right about
// (KEY-003, forge.Secrets.Redact).
func waitForge(ctx context.Context, runner forge.Runner, secrets forge.Secrets) error {
	err := waitReady(ctx, runner, forgeReadyTimeout, secrets,
		probe{
			command:    execProbe(forge.Service),
			waitingFor: "the forgejo container did not start",
		},
		probe{
			command:    forge.ReadyCommand(),
			waitingFor: "forgejo did not finish setting up its database",
		},
	)
	if err == nil || ctx.Err() != nil {
		return err
	}
	if tail := forgeLogTail(ctx, runner, secrets); tail != "" {
		return fmt.Errorf("%w\nthe last %d lines of the forgejo container's log:\n%s", err, logTailLines, tail)
	}
	return err
}

// forgeLogTail is the tail of the forgejo container's log, redacted, or ""
// if there is none to be had. A log that could not be read is reported as no
// log rather than as an empty one: `docker compose logs` failing puts
// Docker's complaint on stderr, and passing that off as the container's
// output would be a lie in the one place the operator is looking for the
// truth.
func forgeLogTail(ctx context.Context, runner forge.Runner, secrets forge.Secrets) string {
	var stdout, stderr bytes.Buffer
	command := fmt.Sprintf("docker compose logs --tail=%d %s", logTailLines, forge.Service)
	if err := runner.Run(ctx, command, &stdout, &stderr); err != nil {
		return ""
	}
	// Both streams again, and for a second reason on top of forge.
	// FailureDetail's: `docker compose logs` relays the container's own
	// stdout and stderr, and which one a Forgejo boot failure lands on is
	// not something to depend on.
	parts := make([]string, 0, 2)
	for _, buf := range []*bytes.Buffer{&stdout, &stderr} {
		if msg := strings.TrimSpace(buf.String()); msg != "" {
			parts = append(parts, msg)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return secrets.Redact(strings.Join(parts, "\n"))
}
