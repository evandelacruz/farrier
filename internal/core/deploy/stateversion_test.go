package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
)

// These tests cover UPGR-003 from `up`'s side: schema migrations run during
// `upgrade` and at no other time, and the only way `up` could run one is by
// starting Forgejo on a version other than the one this host's state was
// last started under.

const testRemoteDir = "/opt/farrier"

// otherForgeImage is a forge image deliberately different from the one
// testBundle pins, standing in for a hand-edited farrier.yaml.
var otherForgeImage = "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("f", 64)

// pinnedForgeImage returns the forge image b's manifest pins.
func pinnedForgeImage(t *testing.T, b *bundle.Bundle) string {
	t.Helper()
	image, ok := b.Manifest.Images[forge.Service]
	if !ok {
		t.Fatal("bundle manifest pins no forgejo image")
	}
	return image
}

// recordedVersion returns the forge version recorded on host, trimmed.
func recordedVersion(host *fakeHost) string {
	return strings.TrimSpace(host.files[StateVersionPath(testRemoteDir)])
}

func TestUpRecordsPinnedVersionOnAHostWithNoRecord(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions(testRemoteDir)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if got, want := recordedVersion(host), pinnedForgeImage(t, b); got != want {
		t.Fatalf("recorded forge version = %q, want %q", got, want)
	}
}

func TestUpRecordsPinnedVersionBeforeConverging(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions(testRemoteDir)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Converge is what starts Forgejo on the pinned image, and starting is
	// what migrates. A record written afterward would leave a converge that
	// died partway looking, to the next `up`, like a host still on the old
	// version — and that `up` would then migrate without noticing.
	if got, want := strings.TrimSpace(host.versionAtConverge), pinnedForgeImage(t, b); got != want {
		t.Fatalf("recorded forge version at converge = %q, want %q", got, want)
	}
}

func TestUpProceedsWhenThePinMatchesTheRecord(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	host.files[StateVersionPath(testRemoteDir)] = pinnedForgeImage(t, b) + "\n"

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions(testRemoteDir)); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(host.commandsContaining("docker compose up")) == 0 {
		t.Fatal("Up did not converge the host")
	}
}

func TestUpRefusesToStartADifferentForgejoVersion(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	host.files[StateVersionPath(testRemoteDir)] = otherForgeImage + "\n"

	job := events.NewJob()
	err := Up(context.Background(), job, host, b, testOptions(testRemoteDir))
	if err == nil {
		t.Fatal("Up succeeded against state last started under a different forgejo version; want refusal")
	}

	// The refusal has to be actionable: both versions, and the one command
	// allowed to change them.
	for _, want := range []string{otherForgeImage, pinnedForgeImage(t, b), "farrier upgrade", "UPGR-003"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}

	// Refusing after converging would be no refusal at all: the migration
	// happens when the container starts.
	if got := host.commandsContaining("docker compose up"); len(got) != 0 {
		t.Fatalf("Up converged the host despite refusing: %v", got)
	}
	if got := recordedVersion(host); got != otherForgeImage {
		t.Fatalf("recorded forge version = %q, want it left at %q", got, otherForgeImage)
	}

	if state := stepState(drain(job), StepCheckVersion); state != events.StateFailed {
		t.Fatalf("%s reported %q, want %q", StepCheckVersion, state, events.StateFailed)
	}
}

func TestUpWithMigrateStartsADifferentForgejoVersion(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	host.files[StateVersionPath(testRemoteDir)] = otherForgeImage + "\n"

	opts := testOptions(testRemoteDir)
	opts.Migrate = true
	if err := Up(context.Background(), events.NewJob(), host, b, opts); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if got, want := recordedVersion(host), pinnedForgeImage(t, b); got != want {
		t.Fatalf("recorded forge version = %q, want %q", got, want)
	}
}

func TestUpRefusesWhenTheRecordCannotBeRead(t *testing.T) {
	host := newFakeHost()
	host.readVersionErr = errors.New("permission denied")

	err := Up(context.Background(), events.NewJob(), host, testBundle(t), testOptions(testRemoteDir))
	if err == nil {
		t.Fatal("Up succeeded with an unreadable version record; want refusal")
	}
	// Fail-closed: an unreadable record is not an absent one, and reading it
	// as absent is how a migration would slip through.
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("refusal does not carry the read failure: %v", err)
	}
	if got := host.commandsContaining("docker compose up"); len(got) != 0 {
		t.Fatalf("Up converged the host despite an unreadable record: %v", got)
	}
}

// commandsContaining returns every command run on the fake host containing
// substr.
func (f *fakeHost) commandsContaining(substr string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.commands {
		if strings.Contains(c, substr) {
			out = append(out, c)
		}
	}
	return out
}

// stepState returns the last state reported for step, or "" if the step
// never reported.
func stepState(evs []events.Event, step string) events.State {
	var state events.State
	for _, ev := range evs {
		if ev.Step == step {
			state = ev.State
		}
	}
	return state
}
