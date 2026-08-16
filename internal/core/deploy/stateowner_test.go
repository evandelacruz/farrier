package deploy

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
)

// These tests cover UP-008: `up` refuses to deploy a bundle onto host state
// that belongs to a different instance, names what it found, changes
// nothing, and stays safe to repeat against state that is its own (UP-003).

// otherInstanceKey is an SSH host public key deliberately different from the
// one testBundle's keystore holds, standing in for a second instance that
// got to this directory first.
const otherInstanceKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIanotherinstancekeyblob other@instance"

// bundleHostKey is the public half of the SSH host key b's keystore holds,
// normalized the way the owner record stores it.
func bundleHostKey(t *testing.T, b *bundle.Bundle) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(keystorePath(t, b), "ssh_host_key.pub"))
	if err != nil {
		t.Fatalf("read bundle ssh host public key: %v", err)
	}
	return normalizeSSHPublicKey(string(data))
}

// installedHostKeyPath is where configureSSHHostKey puts the public half of
// the instance's SSH host key — the fallback ReadStateOwner falls back to on
// a host carrying no owner record.
func installedHostKeyPath(remoteDir string) string {
	return path.Join(GiteaStatePath(remoteDir), sshHostKeyRelPath()) + ".pub"
}

// recordedOwner returns the owner record on host, parsed.
func recordedOwner(host *fakeHost) StateOwner {
	return parseStateOwner(host.files[StateOwnerPath(testRemoteDir)])
}

func TestUpClaimsUnownedState(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions(testRemoteDir)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	owner := recordedOwner(host)
	if got, want := owner.SSHHostKey, bundleHostKey(t, b); got != want {
		t.Errorf("recorded ssh host key = %q, want %q", got, want)
	}
	if got, want := owner.Domain, b.Manifest.Domain; got != want {
		t.Errorf("recorded domain = %q, want %q", got, want)
	}
}

func TestUpClaimsStateBeforeConfiguringAnything(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions(testRemoteDir)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// A deployment that dies partway has still written this bundle's app.ini
	// and key material into that directory, so the claim has to be in place
	// before the first of them lands — otherwise a half-deployed directory
	// looks free to the next bundle pointed at it.
	if got := host.firstWrite; got != StateOwnerPath(testRemoteDir) {
		t.Errorf("first file written to the host = %q, want the owner record %q", got, StateOwnerPath(testRemoteDir))
	}
}

func TestUpProceedsAgainstItsOwnState(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	host.files[StateOwnerPath(testRemoteDir)] = string(StateOwner{
		SSHHostKey: bundleHostKey(t, b),
		Domain:     b.Manifest.Domain,
	}.record())

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions(testRemoteDir)); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(host.commandsContaining("docker compose up")) == 0 {
		t.Fatal("Up did not converge the host")
	}
}

// A domain the record does not carry is not a different instance: `attach`
// (UP-007) fills a name into a nameless bundle in place, on a host that is
// already deployed, and the SSH host key is exactly what does not change
// when it does.
func TestUpProceedsAfterItsInstanceWasNamed(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	host.files[StateOwnerPath(testRemoteDir)] = string(StateOwner{SSHHostKey: bundleHostKey(t, b)}.record())

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions(testRemoteDir)); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if got, want := recordedOwner(host).Domain, b.Manifest.Domain; got != want {
		t.Errorf("recorded domain after the name was attached = %q, want %q", got, want)
	}
}

func TestUpRefusesStateBelongingToAnotherInstance(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	host.files[StateOwnerPath(testRemoteDir)] = string(StateOwner{
		SSHHostKey: otherInstanceKey,
		Domain:     "git.other.example",
	}.record())

	job := events.NewJob()
	err := Up(context.Background(), job, host, b, testOptions(testRemoteDir))
	if err == nil {
		t.Fatal("Up succeeded against another instance's state; want refusal")
	}

	// The refusal has to name what it found and what this deployment is, or
	// the operator cannot tell which of their bundles is in that directory.
	for _, want := range []string{
		"git.other.example",
		normalizeSSHPublicKey(otherInstanceKey),
		b.Manifest.Domain,
		bundleHostKey(t, b),
		path.Join(testRemoteDir, stateDir),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}

	// And it must change nothing: not the record it read, not a file, not a
	// container.
	if got := recordedOwner(host).SSHHostKey; got != normalizeSSHPublicKey(otherInstanceKey) {
		t.Errorf("owner record = %q, want it left at the other instance's key", got)
	}
	if len(host.files) != 1 {
		t.Errorf("Up wrote to the host despite refusing: %v", host.files)
	}
	for _, command := range []string{"docker compose up", "mkdir -p", "chown"} {
		if got := host.commandsContaining(command); len(got) != 0 {
			t.Errorf("Up ran %q despite refusing: %v", command, got)
		}
	}

	if state := stepState(drain(job), StepCheckOwner); state != events.StateFailed {
		t.Errorf("%s reported %q, want %q", StepCheckOwner, state, events.StateFailed)
	}
}

// The population UP-008 exists for was deployed by a binary that wrote no
// owner record at all, so the SSH host key `up` installs under the gitea
// state directory is what identifies those instances — and it identifies
// them on the very first `up` that carries this check, not one re-deploy
// later.
func TestUpRefusesStateCarryingAnotherInstancesInstalledHostKey(t *testing.T) {
	host := newFakeHost()
	host.files[installedHostKeyPath(testRemoteDir)] = otherInstanceKey + "\n"

	err := Up(context.Background(), events.NewJob(), host, testBundle(t), testOptions(testRemoteDir))
	if err == nil {
		t.Fatal("Up succeeded against state serving another instance's ssh host key; want refusal")
	}
	if !strings.Contains(err.Error(), normalizeSSHPublicKey(otherInstanceKey)) {
		t.Errorf("refusal does not name the key it found: %v", err)
	}
	if got := host.commandsContaining("docker compose up"); len(got) != 0 {
		t.Fatalf("Up converged the host despite refusing: %v", got)
	}
}

func TestUpAdoptsStateCarryingItsOwnInstalledHostKey(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	// A live instance of this same bundle, deployed before the record
	// existed: `up` recognizes it and writes the record it was missing.
	host.files[installedHostKeyPath(testRemoteDir)] = bundleHostKey(t, b) + " farrier\n"

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions(testRemoteDir)); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if got, want := recordedOwner(host).SSHHostKey, bundleHostKey(t, b); got != want {
		t.Errorf("recorded ssh host key = %q, want %q", got, want)
	}
}

func TestUpRefusesWhenTheOwnerRecordCannotBeRead(t *testing.T) {
	host := newFakeHost()
	host.readVersionErr = errors.New("permission denied")

	err := Up(context.Background(), events.NewJob(), host, testBundle(t), testOptions(testRemoteDir))
	if err == nil {
		t.Fatal("Up succeeded with an unreadable owner record; want refusal")
	}
	// Fail-closed: an unreadable record is not an absent one, and reading it
	// as absent is how a deployment lands on another instance's state.
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("refusal does not carry the read failure: %v", err)
	}
	if len(host.files) != 0 {
		t.Fatalf("Up wrote to the host despite an unreadable record: %v", host.files)
	}
}

func TestUpClaimsStateForANamelessBundle(t *testing.T) {
	host := newFakeHost()
	b := namelessBundle(t)

	opts := testOptions(testRemoteDir)
	opts.Address = "192.0.2.10"
	if err := Up(context.Background(), events.NewJob(), host, b, opts); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// A nameless instance has no domain to record, and is exactly the case a
	// key-based identity exists for: it still owns its state.
	owner := recordedOwner(host)
	if got, want := owner.SSHHostKey, bundleHostKey(t, b); got != want {
		t.Errorf("recorded ssh host key = %q, want %q", got, want)
	}
	if owner.Domain != "" {
		t.Errorf("recorded domain = %q, want none for a nameless bundle", owner.Domain)
	}
	if !strings.Contains(owner.Describe(), "unnamed") {
		t.Errorf("Describe() = %q, want it to say the instance has no name", owner.Describe())
	}
}

func TestStateOwnerRecordRoundTrip(t *testing.T) {
	owner := StateOwner{SSHHostKey: normalizeSSHPublicKey(otherInstanceKey), Domain: "git.example.com"}
	if got := parseStateOwner(string(owner.record())); got != owner {
		t.Errorf("round-tripped owner = %+v, want %+v", got, owner)
	}
}

func TestParseStateOwnerIgnoresUnknownFields(t *testing.T) {
	record := "ssh-host-key: " + otherInstanceKey + "\nsome-later-field: whatever\n\n"
	got := parseStateOwner(record)
	if want := normalizeSSHPublicKey(otherInstanceKey); got.SSHHostKey != want {
		t.Errorf("parsed ssh host key = %q, want %q", got.SSHHostKey, want)
	}
	if !got.Known() {
		t.Error("owner parsed from a record with an unknown field is unknown; want it read anyway")
	}
}

func TestNormalizeSSHPublicKeyDropsTheComment(t *testing.T) {
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIblob"
	for _, line := range []string{key, key + " farrier", "  " + key + " someone@laptop\n"} {
		if got := normalizeSSHPublicKey(line); got != key {
			t.Errorf("normalizeSSHPublicKey(%q) = %q, want %q", line, got, key)
		}
	}
	// A line it cannot split is still an identity, not an error: what this
	// compares is which instance a key belongs to, not whether the key is
	// well formed.
	if got := normalizeSSHPublicKey(" odd-key-material \n"); got != "odd-key-material" {
		t.Errorf("normalizeSSHPublicKey of an unsplittable line = %q, want it trimmed and kept", got)
	}
}

func TestUnknownOwnersAreNeverTheSameInstance(t *testing.T) {
	if (StateOwner{}).SameInstance(StateOwner{}) {
		t.Error("two unknown owners compared equal; unknown is the absence of an answer")
	}
}
