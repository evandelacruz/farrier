// options.go builds Options' SSH-backed fields the same way for both the
// `backup` CLI command and POST /backup (BKUP-006): the container-naming
// convention, the host-state path under a bundle's remote directory
// (tech-spec.md "Host state layout"), and the push-hold upstream are
// decision logic, not flag-parsing or request-decoding, so they belong here
// rather than duplicated in cmd/farrier and internal/api (CLAUDE.md "one
// core, thin skins").
package backup

import (
	"fmt"
	"path"

	"filippo.io/age"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/caddy"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// Host is everything BuildOptions needs from a connected SSH session: the
// same address it dialed (Target, so the git exporter can build its ssh://
// remote URLs) and the ability to run commands over it (state.Runner, for
// every SSH-backed exporter and the push hold). *orchestrate.Client
// satisfies it in production.
type Host interface {
	state.Runner
	Target() orchestrate.Target
}

// BuildOptions wires Options' host-derived fields from host, b, and the
// already-resolved identity, blob adapter, and keystore driver: the
// forgejo and caddy container names ("farrier-"+service), the git state
// root under remoteDir, and the push hold's Caddy upstream. WorkDir and
// Destination pass through unchanged.
func BuildOptions(host Host, b *bundle.Bundle, remoteDir, workDir, destination string, identity *age.X25519Identity, blobs state.BlobExporter, keystoreDriver keystore.Driver) Options {
	t := host.Target()
	forgeContainer := "farrier-" + forge.Service
	caddyContainer := "farrier-" + caddy.Service

	return Options{
		WorkDir:        workDir,
		ForgejoVersion: b.Manifest.Images[forge.Service],
		Destination:    destination,
		Identity:       identity,
		Git: &state.SSHGitExporter{
			Runner: host,
			User:   t.User,
			Host:   t.Host,
			Port:   t.Port,
			Root:   path.Join(remoteDir, "state", "git"),
		},
		GitCapturer: SSHGitCapturer{Runner: host},
		Database: &state.SSHDatabaseExporter{
			Runner:    host,
			Container: forgeContainer,
			Path:      forge.DatabasePath,
		},
		Blobs: blobs,
		Keys:  &state.KeystoreKeyExporter{Driver: keystoreDriver},
		PushHold: CaddyPushHold{
			Runner:    host,
			Container: caddyContainer,
			Domain:    b.Manifest.Domain,
			Upstream:  fmt.Sprintf("%s:%d", forge.Service, forge.HTTPPort),
		},
	}
}
