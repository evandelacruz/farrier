// Package forge renders Forgejo's own configuration from a Farrier bundle:
// app.ini (FORGE-001), admin bootstrap, fork-PR policy, and CI reconciliation
// land here as their requirement IDs are implemented.
package forge

import (
	"fmt"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
)

// Container-side layout of the official Forgejo Docker image. These are
// fixed, not manifest-derived: the Compose definition that ships this
// app.ini (ORCH-002) must mount volumes and run the container consistent
// with these paths.
//
// DataPath and RepoRoot are exported so a caller wiring the host state
// layout that keeps them durable across a container recreation (UP-004,
// tech-spec.md "Host state layout") can bind-mount onto exactly the paths
// this file's rendered app.ini assumes, without duplicating them.
const (
	dataPath = "/data/gitea"
	dbPath   = dataPath + "/gitea.db"
	lfsPath  = dataPath + "/lfs"
	repoRoot = "/data/git/repositories"
	runUser  = "git"
)

// SSHListenPort is the container-side port Forgejo's builtin SSH server
// binds (rendered as SSH_LISTEN_PORT). It is fixed, like HTTPPort: what the
// operator chooses is the *host* port `up` publishes onto this one
// (bundle.Manifest.GitSSHPort, UP-005), and the mapping between the two is
// the Compose definition's job.
//
// It is deliberately not 22. Forgejo's builtin server runs inside the
// container as RUN_USER rather than root, and a non-root process cannot
// bind a privileged port — so a container-side 22 would fail to listen no
// matter which host port were published onto it. The number matching
// bundle.DefaultGitSSHPort is a coincidence of both wanting an
// unprivileged port; the two are independent, and publishing 22 on the host
// works unchanged.
const SSHListenPort = 2222

// DataPath is the container-side directory the official Forgejo image
// stores its SQLite database, LFS objects, attachments, avatars, and CI
// artifacts under (dataPath above) — everything RenderAppINI doesn't give
// its own path, which is everything except the repository root itself.
const DataPath = dataPath

// RepoRoot is the container-side directory RenderAppINI configures as
// Forgejo's git repository root (repoRoot above).
const RepoRoot = repoRoot

// HTTPPort is the container-side port Forgejo's HTTP server listens on.
// Exported so a caller wiring Caddy as the reverse proxy in front of it
// (UP-002) can address this service without duplicating the port number.
const HTTPPort = 3000

// AppINIPath is the container-side path the official Forgejo image loads
// its configuration from. A caller that deploys RenderAppINI's output
// (UP-001) must mount the rendered file here.
const AppINIPath = dataPath + "/conf/app.ini"

// sshHostKeyDir and sshHostKeyFile fix where Forgejo's SSH host key lives
// under DataPath. RenderAppINI sets SSH_SERVER_HOST_KEYS to this path
// explicitly rather than leaving Forgejo to its own default filenames, so
// there is one Farrier-owned container path to install the bundle's
// persisted ed25519 SSH host key at (deploy.configureSSHHostKey, RSTR-004)
// — the same key init generates and every backup and restore carries from
// then on (spec.md "Identity" > "Key material").
const (
	sshHostKeyDir  = "ssh"
	sshHostKeyFile = "farrier_host_ed25519"
)

// SSHHostKeyPath is the container-side path RenderAppINI configures as
// Forgejo's sole SSH host key.
const SSHHostKeyPath = dataPath + "/" + sshHostKeyDir + "/" + sshHostKeyFile

// DatabasePath is dbPath's exported form: the container-side location of
// Forgejo's SQLite database. A caller reaching it from outside this package
// — state.SSHDatabaseExporter, over `docker exec` (BKUP-006) — needs the
// exact path Forgejo itself was configured with, rather than a second,
// possibly drifting copy of the same string.
const DatabasePath = dbPath

// Secrets are the pieces of Forgejo's identity that let app.ini answer every
// question the install wizard would otherwise ask. They are bundle key
// material (spec.md "Identity" > "Key material"): generated once at init and
// carried through every backup and restore. This package only renders them
// into config — it never generates or persists them.
type Secrets struct {
	// SecretKey encrypts sessions and CSRF tokens ([security] SECRET_KEY).
	SecretKey string
	// InternalToken authenticates Forgejo's web process to its internal
	// SSH/API server ([security] INTERNAL_TOKEN).
	InternalToken string
	// LFSJWTSecret signs Git LFS access tokens ([lfs] JWT_SECRET).
	LFSJWTSecret string
}

func (s Secrets) validate() error {
	fields := []struct {
		name  string
		value string
	}{
		{"SecretKey", s.SecretKey},
		{"InternalToken", s.InternalToken},
		{"LFSJWTSecret", s.LFSJWTSecret},
	}
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("forge: %s is required", f.name)
		}
		if strings.ContainsAny(f.value, "\r\n") {
			return fmt.Errorf("forge: %s must not contain newlines", f.name)
		}
	}
	return nil
}

// AppINIOptions carries the deploy-time choices that change what app.ini
// says, as distinct from the manifest and key material, which say what the
// instance *is*. The zero value renders an ordinary production deployment.
type AppINIOptions struct {
	// Quarantine renders the drill-mode overrides (DRIL-002): outbound
	// webhooks, email, and mirrors are turned off in the rendered config
	// regardless of what the restored database has configured. Only
	// internal/core/drill's deploy path sets it, which is why `up` has no
	// flag for it. See RenderAppINI's "Quarantine" section.
	Quarantine bool
}

// RenderAppINI renders a complete Forgejo app.ini from a bundle manifest and
// its resolved key material. The rendered file sets INSTALL_LOCK, so
// Forgejo's install wizard is never presented (FORGE-001): every field the
// wizard would ask for — domain, database, repository root, LFS, and the
// identity secrets — is already answered.
//
// The result is deploy-time configuration for the forge host, not bundle
// content: callers must ship it to the host directly and never write it into
// the bundle directory (KEY-003).
//
// # Git over SSH
//
// The rendered [server] section starts Forgejo's builtin SSH server on
// SSHListenPort inside the container, pins it to the bundle's own host key
// (SSHHostKeyPath, RSTR-004), and advertises it at the bundle domain on the
// host port the manifest declares (UP-005). The caller publishes that host
// port onto SSHListenPort when it converges the host — this file only says
// what the instance is, not how it is exposed — and the two halves read the
// same manifest field, so the clone URL Forgejo displays is the one that
// answers.
//
// # Quarantine
//
// With opts.Quarantine, the rendered file additionally shuts off every way
// Forgejo reaches out on its own — DRIL-002's "outbound webhooks and email
// disabled", and the half of it that config, rather than the network, has
// to enforce.
//
// It is a config override rather than a database edit for the reason
// spec.md "Rehearsal" gives: a drill instance carries production's
// identity, and its database is production's database. The webhook rows,
// mailer settings, and push mirrors in it are real, and rewriting them
// would mean the drill no longer rehearses the snapshot it was handed —
// the one thing a rehearsal exists to prove. Overriding at render time
// leaves the restored state byte-for-byte what the snapshot held while the
// instance running on top of it stays mute.
//
// Four keys, because Forgejo has four independent ways out:
//
//   - [security] DISABLE_WEBHOOKS turns the webhook feature off outright,
//     covering both repository and system webhooks.
//   - [webhook] ALLOWED_HOST_LIST, left empty, is a deny-all host matcher.
//     It is the second lock on the same door: if a future change re-enables
//     the feature, delivery still has nowhere it is permitted to go.
//   - [mailer] ENABLED = false disables outbound email. Forgejo defaults it
//     off and this renderer never turns it on, so the line is a guarantee
//     rather than a change — one a later mailer feature cannot silently
//     take away from a drill.
//   - [mirror] ENABLED = false stops push mirrors. A push mirror is not a
//     notification, but it is outbound, it fires on push, and it writes to
//     production's real remotes with production's credentials — exactly
//     what "the outside world hears nothing" rules out.
//
// Actions stays enabled under quarantine: a drill has to run a smoke CI job
// (DRIL-001), and CI is what the rehearsal is proving.
func RenderAppINI(m *bundle.Manifest, secrets Secrets, opts AppINIOptions) ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("forge: manifest is required")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := secrets.validate(); err != nil {
		return nil, err
	}

	domain := strings.TrimSpace(m.Domain)
	rootURL := fmt.Sprintf("https://%s/", domain)

	var b strings.Builder
	fmt.Fprintf(&b, "APP_NAME = Farrier\n")
	fmt.Fprintf(&b, "RUN_MODE = prod\n")
	fmt.Fprintf(&b, "RUN_USER = %s\n\n", runUser)

	fmt.Fprintf(&b, "[server]\n")
	// http, not https: Caddy terminates TLS (spec.md "What it's built on") and
	// proxies to Forgejo in plaintext. ROOT_URL below is the external https URL.
	fmt.Fprintf(&b, "PROTOCOL = http\n")
	fmt.Fprintf(&b, "DOMAIN = %s\n", domain)
	fmt.Fprintf(&b, "ROOT_URL = %s\n", rootURL)
	fmt.Fprintf(&b, "HTTP_PORT = %d\n", HTTPPort)
	// SSH_DOMAIN and SSH_PORT are what Forgejo renders into the SSH clone
	// URL it displays, and SSH_LISTEN_PORT is where its builtin server
	// actually binds inside the container. They differ on purpose: clients
	// reach the host port the manifest declares (UP-005), and the Compose
	// definition publishes that host port onto SSHListenPort. Advertising
	// the container port instead would display a URL nothing answers on.
	fmt.Fprintf(&b, "SSH_DOMAIN = %s\n", domain)
	fmt.Fprintf(&b, "SSH_PORT = %d\n", m.GitSSHPortOrDefault())
	fmt.Fprintf(&b, "SSH_LISTEN_PORT = %d\n", SSHListenPort)
	fmt.Fprintf(&b, "START_SSH_SERVER = true\n")
	// Pinned to the bundle's own key rather than Forgejo's default
	// filenames, so the host identity clients see is the one init
	// generated and every backup/restore carries (RSTR-004).
	fmt.Fprintf(&b, "SSH_SERVER_HOST_KEYS = %s\n", SSHHostKeyPath)
	fmt.Fprintf(&b, "LFS_START_SERVER = true\n\n")

	fmt.Fprintf(&b, "[database]\n")
	fmt.Fprintf(&b, "DB_TYPE = sqlite3\n")
	fmt.Fprintf(&b, "PATH = %s\n\n", dbPath)

	fmt.Fprintf(&b, "[repository]\n")
	fmt.Fprintf(&b, "ROOT = %s\n\n", repoRoot)

	fmt.Fprintf(&b, "[security]\n")
	fmt.Fprintf(&b, "INSTALL_LOCK = true\n")
	fmt.Fprintf(&b, "SECRET_KEY = %s\n", secrets.SecretKey)
	fmt.Fprintf(&b, "INTERNAL_TOKEN = %s\n", secrets.InternalToken)
	if opts.Quarantine {
		fmt.Fprintf(&b, "DISABLE_WEBHOOKS = true\n")
	}
	fmt.Fprintf(&b, "\n")

	fmt.Fprintf(&b, "[lfs]\n")
	fmt.Fprintf(&b, "PATH = %s\n", lfsPath)
	fmt.Fprintf(&b, "JWT_SECRET = %s\n\n", secrets.LFSJWTSecret)

	if opts.Quarantine {
		fmt.Fprintf(&b, "[webhook]\n")
		fmt.Fprintf(&b, "ALLOWED_HOST_LIST =\n\n")

		fmt.Fprintf(&b, "[mailer]\n")
		fmt.Fprintf(&b, "ENABLED = false\n\n")

		fmt.Fprintf(&b, "[mirror]\n")
		fmt.Fprintf(&b, "ENABLED = false\n\n")
	}

	// Enabling Actions is also the whole of FORGE-003. Forgejo's fork-PR
	// approval gate is unconditional once Actions is on — it exposes no
	// app.ini key, and no per-repository setting, to loosen or disable it —
	// so there is no separate approval field to render here.
	fmt.Fprintf(&b, "[actions]\n")
	fmt.Fprintf(&b, "ENABLED = true\n\n")

	fmt.Fprintf(&b, "[log]\n")
	fmt.Fprintf(&b, "MODE = console\n")
	fmt.Fprintf(&b, "LEVEL = info\n")

	return []byte(b.String()), nil
}
