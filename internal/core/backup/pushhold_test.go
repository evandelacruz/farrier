package backup

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/caddy"
)

func TestNoopPushHoldDoesNothing(t *testing.T) {
	var hold NoopPushHold
	if err := hold.Engage(context.Background()); err != nil {
		t.Fatalf("Engage: %v", err)
	}
	if err := hold.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestCaddyPushHoldEngageReloadsAgainstAHoldConfig(t *testing.T) {
	runner := &fakeRunner{}
	hold := CaddyPushHold{
		Runner:    runner,
		Container: "farrier-caddy",
		Domain:    "forge.example.com",
		Upstream:  "forgejo:3000",
	}

	if err := hold.Engage(context.Background()); err != nil {
		t.Fatalf("Engage: %v", err)
	}

	config, err := caddy.RenderPushHoldCaddyfile("forge.example.com", "forgejo:3000", DefaultPushHoldMessage)
	if err != nil {
		t.Fatalf("RenderPushHoldCaddyfile: %v", err)
	}
	want := expectedReloadCommand(t, "farrier-caddy", holdCaddyfilePath, config)
	if runner.command != want {
		t.Fatalf("command = %q, want %q", runner.command, want)
	}
}

func TestCaddyPushHoldEngageUsesCustomMessage(t *testing.T) {
	runner := &fakeRunner{}
	hold := CaddyPushHold{
		Runner:    runner,
		Container: "farrier-caddy",
		Domain:    "forge.example.com",
		Upstream:  "forgejo:3000",
		Message:   "custom rejection message",
	}

	if err := hold.Engage(context.Background()); err != nil {
		t.Fatalf("Engage: %v", err)
	}

	config, err := caddy.RenderPushHoldCaddyfile("forge.example.com", "forgejo:3000", "custom rejection message")
	if err != nil {
		t.Fatalf("RenderPushHoldCaddyfile: %v", err)
	}
	want := expectedReloadCommand(t, "farrier-caddy", holdCaddyfilePath, config)
	if runner.command != want {
		t.Fatalf("command = %q, want %q", runner.command, want)
	}
}

func TestCaddyPushHoldReleaseReloadsTheOriginalCaddyfile(t *testing.T) {
	runner := &fakeRunner{}
	hold := CaddyPushHold{
		Runner:    runner,
		Container: "farrier-caddy",
		Domain:    "forge.example.com",
		Upstream:  "forgejo:3000",
	}

	if err := hold.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}

	want := expectedReloadCommand(t, "farrier-caddy", caddy.ConfigPath, nil)
	if runner.command != want {
		t.Fatalf("command = %q, want %q", runner.command, want)
	}
}

func TestCaddyPushHoldRequiresRunner(t *testing.T) {
	hold := CaddyPushHold{Container: "farrier-caddy"}
	if err := hold.Engage(context.Background()); err == nil {
		t.Fatal("Engage: want error with no runner, got nil")
	}
	if err := hold.Release(context.Background()); err == nil {
		t.Fatal("Release: want error with no runner, got nil")
	}
}

func TestCaddyPushHoldRequiresContainer(t *testing.T) {
	hold := CaddyPushHold{Runner: &fakeRunner{}}
	if err := hold.Engage(context.Background()); err == nil {
		t.Fatal("Engage: want error with no container, got nil")
	}
}

func TestCaddyPushHoldPropagatesReloadError(t *testing.T) {
	runner := &fakeRunner{err: errors.New("exit status 1"), stderr: "unknown directive"}
	hold := CaddyPushHold{Runner: runner, Container: "farrier-caddy", Domain: "forge.example.com", Upstream: "forgejo:3000"}

	err := hold.Engage(context.Background())
	if err == nil {
		t.Fatal("Engage: want error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown directive") {
		t.Errorf("error = %v, want it to carry the runner's stderr", err)
	}
}

// expectedReloadCommand mirrors CaddyPushHold.reload's own construction, so
// tests assert on the actual shape of the command rather than duplicating
// its escaping logic ad hoc.
func expectedReloadCommand(t *testing.T, container, path string, config []byte) string {
	t.Helper()
	var script strings.Builder
	if config != nil {
		encoded := base64.StdEncoding.EncodeToString(config)
		fmt.Fprintf(&script, "printf '%%s' %s | base64 -d > %s && ", gitShellQuote(encoded), gitShellQuote(path))
	}
	fmt.Fprintf(&script, "caddy reload --config %s --adapter caddyfile", gitShellQuote(path))
	return fmt.Sprintf("docker exec %s sh -c %s", gitShellQuote(container), gitShellQuote(script.String()))
}
