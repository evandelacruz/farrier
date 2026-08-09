package deploy

import (
	"context"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/events"
)

// TestDownRemovesTheDeployment is DRIL-003's core mechanic: after an Up and
// a Down against the same host, the Compose project is gone and so is
// RemoteDir — which is where UP-004 deliberately puts forge state, outside
// any container and so out of reach of `docker compose down` alone.
func TestDownRemovesTheDeployment(t *testing.T) {
	host := newFakeHost()
	b := testBundle(t)
	const remoteDir = "/opt/farrier"

	if err := Up(context.Background(), events.NewJob(), host, b, testOptions(remoteDir)); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := Down(context.Background(), host, b, remoteDir); err != nil {
		t.Fatalf("Down: %v", err)
	}

	downs := host.commandsContaining("docker compose down")
	if len(downs) != 1 {
		t.Fatalf("want exactly one compose down command, got %v", downs)
	}
	for _, want := range []string{"--volumes", "--remove-orphans"} {
		if !strings.Contains(downs[0], want) {
			t.Errorf("compose down command %q does not carry %s", downs[0], want)
		}
	}

	// The project torn down is the project Converge started: same name,
	// same file list, same working directory.
	converge := host.commandsContaining("docker compose up -d --remove-orphans")
	if len(converge) != 1 {
		t.Fatalf("want exactly one converge command, got %v", converge)
	}
	prefix, _, _ := strings.Cut(converge[0], " docker compose up")
	if !strings.Contains(downs[0], prefix) {
		t.Errorf("compose down command %q does not address the project converge started with %q", downs[0], prefix)
	}

	// RemoteDir itself is removed, which is the only thing that takes the
	// bind-mounted state with it.
	removals := host.commandsContaining("rm -rf '" + remoteDir + "'")
	if len(removals) != 1 {
		t.Fatalf("want exactly one removal of %s, got %v", remoteDir, removals)
	}
	for _, statePath := range []string{GitStatePath(remoteDir), GiteaStatePath(remoteDir), StateVersionPath(remoteDir)} {
		if !strings.HasPrefix(statePath, remoteDir+"/") {
			t.Errorf("%s is not under %s, so removing %s would not remove it", statePath, remoteDir, remoteDir)
		}
	}

	// Containers first, files second: removing the directory out from under
	// a still-running container is not a teardown.
	if down, remove := indexOfCommandContaining(host, "docker compose down"), indexOfCommandContaining(host, "rm -rf '"+remoteDir+"'"); down > remove {
		t.Errorf("compose down ran at %d, after the removal at %d", down, remove)
	}
}

// TestDownSkipsComposeOnAHostThatNeverConverged covers the exit path a
// drill that failed early takes: nothing was ever shipped to the host, so
// there is no Compose project to stop, and the teardown must not report a
// failure for one.
func TestDownSkipsComposeOnAHostThatNeverConverged(t *testing.T) {
	host := newFakeHost()
	const remoteDir = "/opt/farrier"

	if err := Down(context.Background(), host, testBundle(t), remoteDir); err != nil {
		t.Fatalf("Down: %v", err)
	}

	downs := host.commandsContaining("docker compose down")
	if len(downs) != 1 {
		t.Fatalf("want exactly one compose down command, got %v", downs)
	}
	if !strings.HasPrefix(downs[0], "if [ -d '"+remoteDir+"/compose' ]; then ") {
		t.Errorf("compose down command %q is not guarded on the shipped compose directory", downs[0])
	}
	if len(host.commandsContaining("rm -rf '"+remoteDir+"'")) != 1 {
		t.Errorf("want %s removed even with nothing converged, got %v", remoteDir, host.commands)
	}
}

// TestDownReportsAFailedComposeDown pins that a teardown failure is
// reported rather than swallowed, and that Down stops there: with
// containers still running against RemoteDir's bind mounts, removing the
// directory would leave a worse mess than the one it was called to clean.
func TestDownReportsAFailedComposeDown(t *testing.T) {
	host := newFakeHost()
	host.failOutputOn = "docker compose down"
	const remoteDir = "/opt/farrier"

	err := Down(context.Background(), host, testBundle(t), remoteDir)
	if err == nil {
		t.Fatal("Down: want an error when the compose project cannot be stopped")
	}
	if !strings.Contains(err.Error(), "docker compose down") {
		t.Errorf("error %q does not name the step that failed", err)
	}
	if removals := host.commandsContaining("rm -rf '" + remoteDir + "'"); len(removals) != 0 {
		t.Errorf("want no removal after a failed compose down, got %v", removals)
	}
}

// TestDownReportsAFailedRemoval covers the other half: the containers are
// gone but the files are not, which still leaves the host holding the
// deployment's state.
func TestDownReportsAFailedRemoval(t *testing.T) {
	host := newFakeHost()
	host.failOutputOn = "rm -rf"
	const remoteDir = "/opt/farrier"

	err := Down(context.Background(), host, testBundle(t), remoteDir)
	if err == nil {
		t.Fatal("Down: want an error when the remote directory cannot be removed")
	}
	if !strings.Contains(err.Error(), remoteDir) {
		t.Errorf("error %q does not name the directory left behind", err)
	}
}

func TestDownValidatesItsArguments(t *testing.T) {
	tests := []struct {
		name      string
		host      Host
		remoteDir string
		bundle    bool
		want      string
	}{
		{name: "no host", remoteDir: "/opt/farrier", bundle: true, want: "host is required"},
		{name: "no remote directory", host: newFakeHost(), remoteDir: "  ", bundle: true, want: "remote directory is required"},
		{name: "no bundle", host: newFakeHost(), remoteDir: "/opt/farrier", want: "no rendered Compose files"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := testBundle(t)
			if !tt.bundle {
				b = nil
			}
			err := Down(context.Background(), tt.host, b, tt.remoteDir)
			if err == nil {
				t.Fatal("Down: want an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}
