package restore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// UPGR-003 says schema migrations run during `upgrade` and at no other
// time. Restore is the path that could most plausibly break that: it boots a
// Forgejo instance, and the host it boots on may already have carried a
// different version — a standby that was up before, or a scratch target
// reused between drills.
//
// It doesn't, and these tests are why: restore places the snapshot's
// database and stamps the version that wrote it (RSTR-002, spec.md "Version
// pinning"), then boots exactly that version, so the pair Forgejo compares
// on startup — image against database — is the pair the snapshot was
// captured as. There is nothing to migrate, and deploy.Up's own UPGR-003
// check sees a match rather than being handed an exemption.

// seedTLSForDeploy installs the snapshot's TLS pair into the target keystore
// so deploy.Up's configureTLS has a certificate to reuse (fakeCertIssuer
// refuses to invent one) — the same setup TestRestoreEndToEnd does.
func seedTLSForDeploy(t *testing.T, opts Options) {
	t.Helper()
	values := testKeyValues()
	keysDir := opts.Bundle.Manifest.Drivers.Keystore.Config["path"].(string)
	for _, name := range []string{state.KeyTLSCertificate, state.KeyTLSPrivateKey} {
		if err := os.WriteFile(filepath.Join(keysDir, name), []byte(values[name]), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
}

func TestRestoreDoesNotMigrate(t *testing.T) {
	opts := validOptions(t)
	seedTLSForDeploy(t, opts)

	host := opts.Host.(*fakeHost)
	// The host already carries state from a different Forgejo version — the
	// case that would make an `up` refuse. A restore replaces that state
	// wholesale, so it is not a migration and must go through.
	versionPath := deploy.StateVersionPath(opts.RemoteDir)
	host.files[versionPath] = testTargetForgeImage + "\n"

	if err := Restore(context.Background(), events.NewJob(), opts); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// The record now names the version that wrote the database that is
	// actually on the host: the snapshot's.
	if got, want := strings.TrimSpace(host.files[versionPath]), testSnapshotForgeImage; got != want {
		t.Fatalf("recorded forge version after restore = %q, want the snapshot's %q", got, want)
	}

	// And that is the version the host was converged to, so Forgejo starts
	// on exactly the version its database was written by.
	compose := host.fileEndingWith(t, "docker-compose.yml")
	if !strings.Contains(compose, testSnapshotForgeImage) {
		t.Fatalf("shipped compose does not pin the snapshot's forgejo image %q:\n%s", testSnapshotForgeImage, compose)
	}
}

func TestRestoreStampsTheVersionBeforeConverging(t *testing.T) {
	opts := validOptions(t)
	seedTLSForDeploy(t, opts)

	host := opts.Host.(*fakeHost)
	if err := Restore(context.Background(), events.NewJob(), opts); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Ordering is the whole guarantee: deploy.Up reads the record to decide
	// whether it is about to migrate, so restore has to have written it by
	// then. Both the database it describes and the record itself land before
	// the converge that starts Forgejo.
	converge := commandIndex(host, "docker compose up -d --remove-orphans")
	if converge < 0 {
		t.Fatal("host was never converged")
	}
	read := commandIndex(host, deploy.StateVersionPath(opts.RemoteDir))
	if read < 0 {
		t.Fatal("the recorded forge version was never read")
	}
	if read > converge {
		t.Fatalf("version record read at command %d, after converge at %d", read, converge)
	}
}

// commandIndex returns the position of the first command on host containing
// substr, or -1.
func commandIndex(host *fakeHost, substr string) int {
	host.mu.Lock()
	defer host.mu.Unlock()
	for i, c := range host.commands {
		if strings.Contains(c, substr) {
			return i
		}
	}
	return -1
}
