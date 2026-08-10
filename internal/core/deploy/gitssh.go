package deploy

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// publishGitSSH returns compose with the forgejo service's builtin SSH
// server published on the host port the bundle's manifest declares
// (bundle.Manifest.GitSSHPortOrDefault, defaulting to 2222), mapped onto
// the container-side port that server binds (forge.SSHListenPort) — the
// deploy-side half of UP-005.
//
// The other half is the app.ini forge.RenderAppINI shipped a step earlier:
// it advertises the same manifest port as SSH_PORT, so the SSH clone URL
// Forgejo displays is the port published here. Both read
// GitSSHPortOrDefault rather than each carrying a default of its own, which
// is what makes "the URL Forgejo displays is the one that works" a
// structural property instead of a coincidence two files have to keep
// agreeing on.
//
// Nothing here is host-specific or first-deploy-specific: the port comes
// from the manifest, and the key Forgejo presents on it is the bundle's
// (configureSSHHostKey). A restored or promoted instance therefore answers
// on the same port with the same key without restore or promote doing
// anything special — both reach the host through this same Up (RSTR-004).
//
// quarantine binds the published port to the deploy host's loopback
// interface instead of every interface, exactly as configureTLS does for
// Caddy's HTTPS port: a drilled instance carries production's identity and
// production's SSH host key, so a routable git-over-SSH port on a scratch
// host would be a second endpoint answering as production — the thing
// DRIL-002's "reachable only through an SSH tunnel" rules out. Git over SSH
// on a drill is reached through a tunnel to the scratch host, or not at all.
func publishGitSSH(compose map[string][]byte, m *bundle.Manifest, quarantine bool) (map[string][]byte, error) {
	hostPort := strconv.Itoa(m.GitSSHPortOrDefault())
	containerPort := strconv.Itoa(forge.SSHListenPort)

	publish := orchestrate.WithPorts
	if quarantine {
		publish = orchestrate.WithLoopbackPorts
	}

	compose, err := publish(compose, forge.Service, hostPort, containerPort)
	if err != nil {
		return nil, fmt.Errorf("publish git-over-ssh port: %w", err)
	}
	return compose, nil
}

// gitSSHDetail is the event detail publishGitSSH's step reports (CORE-002):
// the clone URL the operator can actually use, spelled the way Forgejo will
// display it. The spelling comes from bundle.Manifest.GitSSHCloneURLAt, the
// same function `publish` writes into a project's `origin` (IMPT-004), so
// the URL reported here and the URL configured there cannot diverge.
//
// address is the operator-supplied address a nameless bundle is served at
// (UP-006) and is empty for a named one, which is addressed at its domain.
// It changes the host in the clone URL and nothing else — the port, the
// key, and the published mapping are identical either way, which is
// UP-006's "with git over SSH unchanged".
//
// It names no key material — the host key is identified by where it came
// from, never by its bytes (KEY-003).
func gitSSHDetail(m *bundle.Manifest, address string, quarantine bool) string {
	port := m.GitSSHPortOrDefault()

	host := strings.TrimSpace(m.Domain)
	if !m.Named() {
		host = address
	}
	clone := m.GitSSHCloneURLAt(host, "<owner>", "<repo>")
	if quarantine {
		return fmt.Sprintf("ssh host key installed; git over SSH published on %s:%d only, reachable through an SSH tunnel (DRIL-002)", orchestrate.LoopbackAddress, port)
	}
	return fmt.Sprintf("ssh host key installed; git over SSH published on host port %d — clone with %s", port, clone)
}
