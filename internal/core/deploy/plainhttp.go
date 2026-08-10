package deploy

import (
	"context"
	"fmt"
	"path"
	"strconv"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/caddy"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// containerHTTPPort is the container-side port Caddy's plain-HTTP site
// binds (caddy.HTTPPort), spelled for a Compose port mapping — the
// counterpart to containerHTTPSPort. The host side is the manifest's
// (publishedWebPort), and defaults to bundle.DefaultNamelessWebPort rather
// than 80 because 80 is the port a developer's own machine is most likely
// to have taken already.
var containerHTTPPort = strconv.Itoa(caddy.HTTPPort)

// configurePlainHTTP renders Caddy's config for a nameless bundle, ships
// it to host, and returns compose with Caddy's bind mount and its published
// HTTP port added (UP-006).
//
// It is configureTLS's counterpart and is deliberately much smaller,
// because everything configureTLS does beyond rendering exists to serve a
// certificate: there is no certificate to resolve from the keystore, none
// to renew, none to persist back, and no ACME account key to generate.
// A nameless bundle proved no zone at init (INIT-005) and holds no TLS key
// material at all, so a shared code path would spend its first three steps
// resolving things that are not there.
//
// Caddy still terminates — the operator's browser reaches Caddy, and Caddy
// proxies to Forgejo on the Compose network, exactly as in the named case.
// What changes is only that the outer hop is unencrypted, which is the
// trade spec.md "Instances without a name" records and the operator
// accepted at `init`. The caller states that plainly through the event
// stream (plainHTTPDetail); it is never left for the operator to infer
// from a missing certificate.
//
// It writes to the same caddyConfigDir/Caddyfile path configureTLS does,
// so an instance that later attaches a name (UP-007) overwrites this
// config rather than leaving two behind, and a re-run overwrites its own
// (UP-003). The certificate and key files configureTLS ships are simply
// never written, and the rendered config never references them.
func configurePlainHTTP(ctx context.Context, host Host, m *bundle.Manifest, remoteDir string, compose map[string][]byte, address string) (map[string][]byte, error) {
	upstream := fmt.Sprintf("%s:%d", forge.Service, forge.HTTPPort)
	caddyfile, err := caddy.RenderPlainHTTPCaddyfile(address, upstream)
	if err != nil {
		return nil, fmt.Errorf("render caddyfile: %w", err)
	}

	caddyfilePath := path.Join(remoteDir, caddyConfigDir, caddyfileFilename)
	if err := host.WriteFile(ctx, caddyfilePath, caddyfile, 0o644); err != nil {
		return nil, fmt.Errorf("ship caddyfile: %w", err)
	}

	compose, err = orchestrate.WithBindMount(compose, caddy.Service, caddyfilePath, caddy.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("mount caddyfile: %w", err)
	}
	compose, err = orchestrate.WithPorts(compose, caddy.Service, publishedWebPort(m), containerHTTPPort)
	if err != nil {
		return nil, fmt.Errorf("publish http port: %w", err)
	}
	return compose, nil
}

// plainHTTPDetail is the event detail the plain-HTTP step reports
// (CORE-002), and it is the whole of UP-006's "must state through the event
// stream that the web UI is unencrypted and belongs on a trusted network."
//
// It goes through the event stream rather than into a CLI print so that
// both frontends carry it — the dashboard renders the same events the CLI
// prints (CLAUDE.md "One core, thin skins") — and it says what is exposed
// and what to do about it, in the same words docs/security.md "A nameless
// instance serves its web UI in the clear" uses. Naming git over SSH here
// is not padding: the operator's next question after "unencrypted" is
// whether pushing is safe, and the answer is yes.
func plainHTTPDetail(m *bundle.Manifest, address string) string {
	return fmt.Sprintf("caddy configured to serve %s — the web UI is unencrypted, so pull requests, review, and login travel in the clear: keep this instance on a trusted network (a LAN, a VPN, or a tailnet). Git over SSH is encrypted regardless — see docs/security.md", forge.InstanceURL(m, address))
}

// caddyReadyDetail is the event detail Up's final step reports: the URL the
// operator can open now that Caddy is answering. A nameless instance
// carries the unencrypted warning a second time, deliberately — this is the
// line an operator reads at the end of a long stream, and the URL and its
// caveat belong in the same sentence.
func caddyReadyDetail(m *bundle.Manifest, address string) string {
	url := forge.InstanceURL(m, address)
	if m.Named() {
		return fmt.Sprintf("caddy ready — %s is live", url)
	}
	return fmt.Sprintf("caddy ready — %s is live, unencrypted: trusted networks only", url)
}
