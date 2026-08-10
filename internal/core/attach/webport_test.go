package attach

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/acme"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/orchestrate"
)

// Naming an instance changes what its published web port means: the
// nameless tier defaults to 8222 and the named one to 443, so attach has to
// recompute both the port it publishes and the URL it reports (UP-007).

// The ordinary case. An instance that only ever had the nameless default
// moves to the named one, and the URLs on both sides of the report say so.
func TestAttachMovesTheDefaultPublishedPortToTheNamedDefault(t *testing.T) {
	dir, _ := namelessBundleDir(t)
	host := newFakeHost()
	job := events.NewJob()

	seen, err := details(t, job, func() error {
		named, err := Attach(context.Background(), job, testOptions(t, dir, host))
		if err != nil {
			return err
		}
		if got := named.Manifest.WebPortOrDefault(); got != bundle.DefaultNamedWebPort {
			t.Errorf("published port after naming = %d, want %d", got, bundle.DefaultNamedWebPort)
		}
		if got, want := named.Manifest.PublicURL(), "https://"+newDomain+"/"; got != want {
			t.Errorf("public URL after naming = %q, want %q", got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// The manifest on disk carries the move, so `up`, `status`, and
	// `backup` all see the same instance the report described.
	reloaded, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("reload named bundle: %v", err)
	}
	if got := reloaded.Manifest.WebPortOrDefault(); got != bundle.DefaultNamedWebPort {
		t.Errorf("saved manifest publishes %d, want %d", got, bundle.DefaultNamedWebPort)
	}

	want := fmt.Sprintf("web UI: http://%s:%d/ → https://%s/", oldAddress, bundle.DefaultNamelessWebPort, newDomain)
	if !containing(seen, want) {
		t.Errorf("event stream never reported %q; details were:\n%s", want, strings.Join(seen, "\n"))
	}
}

// A port the operator picked is theirs, and survives being named. What
// changes is the scheme and the identity, not a decision they made about
// their own host.
func TestAttachKeepsAPortTheOperatorChose(t *testing.T) {
	dir, _ := namelessBundleDir(t)
	setWebPorts(t, dir, 9443, 9443)
	job := events.NewJob()

	seen, err := details(t, job, func() error {
		named, err := Attach(context.Background(), job, testOptions(t, dir, newFakeHost()))
		if err != nil {
			return err
		}
		if got := named.Manifest.WebPortOrDefault(); got != 9443 {
			t.Errorf("published port after naming = %d, want the operator's 9443", got)
		}
		if got, want := named.Manifest.PublicURL(), "https://"+newDomain+":9443/"; got != want {
			t.Errorf("public URL after naming = %q, want %q", got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	want := fmt.Sprintf("web UI: http://%s:9443/ → https://%s:9443/", oldAddress, newDomain)
	if !containing(seen, want) {
		t.Errorf("event stream never reported %q; details were:\n%s", want, strings.Join(seen, "\n"))
	}
}

// A nameless bundle needs no public port; a named one on a moved port does.
// Attach is where that requirement starts applying, so it refuses at
// validation — before an ACME exchange is spent and before the bundle on
// disk is named.
func TestAttachRefusesWhenNamingWouldLeaveThePublicPortUnstated(t *testing.T) {
	dir, _ := namelessBundleDir(t)
	setWebPorts(t, dir, 9443, 0)
	host := newFakeHost()

	prover := &recordingProver{}
	opts := testOptions(t, dir, host)
	opts.Prover = prover

	_, err := Attach(context.Background(), events.NewJob(), opts)
	if err == nil {
		t.Fatal("Attach: want a refusal, got nil")
	}
	if !strings.Contains(err.Error(), "publicWebPort") {
		t.Errorf("refusal = %q, want it to name the field that resolves it", err)
	}
	if prover.called {
		t.Error("a refused attach still spent an ACME exchange")
	}

	reloaded, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("reload bundle: %v", err)
	}
	if reloaded.Manifest.Named() {
		t.Error("a refused attach named the bundle on disk")
	}
	if len(host.files) != 0 {
		t.Errorf("a refused attach wrote %d files to the host", len(host.files))
	}
}

// recordingProver reports whether zone control was ever proven, so a test
// can assert a refusal happened before anything was spent.
type recordingProver struct{ called bool }

func (p *recordingProver) Prove(domain, dnsProvider, email string) (*acme.Certificate, error) {
	p.called = true
	return nil, fmt.Errorf("recordingProver: should not have been called")
}

// setWebPorts rewrites the saved bundle's web ports in place, standing in
// for an operator who set them at `init` or edited farrier.yaml since.
func setWebPorts(t *testing.T, dir string, webPort, publicWebPort int) {
	t.Helper()
	b, err := bundle.Load(dir)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	b.Manifest.WebPort = webPort
	b.Manifest.PublicWebPort = publicWebPort
	compose, err := orchestrate.Render(&b.Manifest)
	if err != nil {
		t.Fatalf("render compose: %v", err)
	}
	b.Compose = compose
	if err := b.Save(dir); err != nil {
		t.Fatalf("save bundle: %v", err)
	}
}
