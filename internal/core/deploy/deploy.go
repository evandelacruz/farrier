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
// it, so recreating that container never destroys it; and UP-005: git over
// SSH is served at the bundle domain on the host port the manifest
// declares, using the bundle's own SSH host key.
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
// StepConfigureTLS and StepConfigureHTTP are alternatives, not a sequence:
// a deployment runs exactly one of them, whichever the bundle calls for
// (UP-002 for a named bundle, UP-006 for a nameless one). They are separate
// identifiers rather than one step with two details so that a frontend
// rendering the stream shows what actually happened — a step named
// "configure-tls" on a deployment that configured no TLS would be a lie
// told by a progress bar.
const (
	StepCheckHost       = "check-host"
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

// readyTimeout bounds how long Up waits for the forgejo container to
// accept `docker compose exec` after Converge starts it.
const readyTimeout = 60 * time.Second

// readyInterval is the delay between readiness probes within readyTimeout.
// A var, not a const, so tests can shorten it rather than actually waiting.
var readyInterval = 2 * time.Second

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
// Forgejo to accept commands, provisions the first admin account, registers
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
// (UP-003): CheckHost and waitReady are read-only, configureForge always
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
// a second one (forge.RegisterRunner).
//
// One step does read the host back before deciding: checkStateVersion
// refuses to start a Forgejo version other than the one the host's state was
// last started under, because that is what makes Forgejo migrate its schema,
// and UPGR-003 puts migrations under `upgrade` alone. Up otherwise lets the
// bundle alone determine the outcome, same as Converge.
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

	job.Started(StepConfigureForge, "resolving key material and rendering app.ini")
	compose, err := configureForge(ctx, host, b, opts.RemoteDir, address, opts.Quarantine)
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

	job.Started(StepWaitForge, "waiting for forgejo to accept commands")
	if err := waitReady(ctx, runner, forge.Service); err != nil {
		job.Emit(StepWaitForge, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: wait for forgejo: %w", err)
	}
	job.Emit(StepWaitForge, events.StateSucceeded, "forgejo ready")

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
	if err := waitReady(ctx, runner, caddy.Service); err != nil {
		job.Emit(StepWaitCaddy, events.StateFailed, err.Error())
		return fmt.Errorf("deploy: wait for caddy: %w", err)
	}
	job.Emit(StepWaitCaddy, events.StateSucceeded, caddyReadyDetail(&b.Manifest, address))
	return nil
}

// configureForge resolves the bundle's key material, renders app.ini,
// ships it to host, and returns b's Compose files with a bind mount added
// so the forgejo service loads the file that was just shipped.
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
func configureForge(ctx context.Context, host Host, b *bundle.Bundle, remoteDir, address string, quarantine bool) (map[string][]byte, error) {
	driver, err := keystore.New(b.Manifest.Drivers.Keystore.Driver, b.Manifest.Drivers.Keystore.Config)
	if err != nil {
		return nil, fmt.Errorf("keystore driver: %w", err)
	}
	secrets, err := forge.ResolveSecrets(ctx, driver)
	if err != nil {
		return nil, fmt.Errorf("resolve key material: %w", err)
	}
	appINI, err := forge.RenderAppINI(&b.Manifest, secrets, forge.AppINIOptions{Quarantine: quarantine, Address: address})
	if err != nil {
		return nil, fmt.Errorf("render app.ini: %w", err)
	}

	hostPath := path.Join(remoteDir, hostConfigDir, appINIFilename)
	if err := host.WriteFile(ctx, hostPath, appINI, 0o600); err != nil {
		return nil, fmt.Errorf("ship app.ini: %w", err)
	}

	compose, err := orchestrate.WithBindMount(b.Compose, forge.Service, hostPath, forge.AppINIPath)
	if err != nil {
		return nil, fmt.Errorf("mount app.ini: %w", err)
	}

	sum := sha256.Sum256(appINI)
	compose, err = orchestrate.WithEnv(compose, forge.Service, appINIChecksumEnv, hex.EncodeToString(sum[:]))
	if err != nil {
		return nil, fmt.Errorf("set app.ini checksum: %w", err)
	}
	return compose, nil
}

// waitReady polls until service accepts `docker compose exec`, or
// readyTimeout elapses. Converge's `docker compose up -d` returns once
// containers are created and started, which can race the container's own
// entrypoint init; admin bootstrap and the browser-readiness check right
// after would then fail intermittently rather than deterministically, so
// this waits the race out instead of leaving it as a heisenbug in the
// caller.
func waitReady(ctx context.Context, runner forge.Runner, service string) error {
	deadline := time.Now().Add(readyTimeout)
	var lastErr error
	for {
		var stderr bytes.Buffer
		err := runner.Run(ctx, fmt.Sprintf("docker compose exec -T %s true", service), io.Discard, &stderr)
		if err == nil {
			return nil
		}
		lastErr = err
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			lastErr = fmt.Errorf("%s", msg)
		}

		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s did not become ready within %s: %w", service, readyTimeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyInterval):
		}
	}
}
