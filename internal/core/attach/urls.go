package attach

import (
	"fmt"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
)

// cloneURLPlaceholders stand in for the owner and repository in the clone
// URLs the report shows. Attach never asks Forgejo what repositories exist:
// the endpoint is what changed, identically for every repository on the
// instance, so a per-repository list would be the same two URLs repeated
// with different tails — and it would make the report depend on a running
// Forgejo at the exact moment the operator most wants to be told what to do
// next. The placeholders are the same ones `up` reports its clone URL with
// (deploy.gitSSHDetail).
const (
	placeholderOwner = "<owner>"
	placeholderRepo  = "<repo>"
)

// reportCloneURLs is UP-007's "report the clone URLs that changed": one
// CORE-002 event per URL that moved, spelling both the address the instance
// answered at before and the domain it answers at now, so the operator can
// hand their team the change without deriving it.
//
// m is the bundle's manifest after naming, and oldAddress is the address it
// was served at before (normalized the way `up` normalized it). Both URLs
// are built by the same functions that produce the real ones — every
// spelling rule about ports, scp-style versus ssh://, and bracketed IPv6
// lives in bundle.Manifest and forge.InstanceURL, so a URL reported here
// cannot drift from the URL Forgejo displays or the one `publish` writes
// into a project's origin (IMPT-004).
//
// One event per line, not one event with three lines in it: the dashboard
// renders an event as a row and the CLI renders it as a line, so a detail
// carrying embedded newlines would read correctly in exactly one of the two
// frontends (initialize.reportKeyMaterial documents the same choice).
func reportCloneURLs(job *events.Job, m *bundle.Manifest, oldAddress string) {
	job.Started(StepReportCloneURLs, "what consumers have to re-point")

	nameless := bundle.Manifest{}
	job.Emit(StepReportCloneURLs, events.StateSucceeded, fmt.Sprintf(
		"web UI: %s → %s",
		forge.InstanceURL(&nameless, oldAddress), forge.InstanceURL(m, ""),
	))

	job.Emit(StepReportCloneURLs, events.StateSucceeded, fmt.Sprintf(
		"git over SSH: %s → %s",
		m.GitSSHCloneURLAt(oldAddress, placeholderOwner, placeholderRepo),
		m.GitSSHCloneURL(placeholderOwner, placeholderRepo),
	))

	job.Emit(StepReportCloneURLs, events.StateSucceeded, fmt.Sprintf(
		"re-point an existing clone with: git remote set-url origin %s",
		m.GitSSHCloneURL(placeholderOwner, placeholderRepo),
	))

	job.Emit(StepReportCloneURLs, events.StateSucceeded, hostKeyDetail)
}

// hostKeyDetail is the sentence that keeps an operator from expecting the
// wrong thing at the other end of the change.
//
// The SSH host key is bundle key material and does not rotate (RSTR-004),
// so nothing about naming an instance changes the key clients are offered.
// What does change is the name that key is filed under: OpenSSH keys
// known_hosts by host and port, so an existing clone that already trusted
// the old address gets an ordinary unknown-host prompt for the new name —
// never the loud host-key-mismatch warning that means an instance was
// rebuilt. The distinction is worth one line, because those two prompts
// look nothing alike to the person receiving them.
const hostKeyDetail = "the SSH host key is unchanged (RSTR-004), so a client that already trusted this instance sees an ordinary unknown-host prompt for the new name — never a host-key mismatch"
