package deploy

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
)

func init() {
	readyInterval = time.Millisecond
}

// fakeHost implements Host without a real SSH server, recording every
// command it's asked to run so tests can assert on Up's sequencing.
type fakeHost struct {
	mu sync.Mutex

	files    map[string]string
	commands []string

	checkHostErr   error
	writeFileErr   error
	execFailures   int // number of leading `docker compose exec ... true` calls that fail
	adminCreateErr error
}

func newFakeHost() *fakeHost {
	return &fakeHost{files: make(map[string]string)}
}

func (f *fakeHost) Output(ctx context.Context, command string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	return nil, nil
}

func (f *fakeHost) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeFileErr != nil {
		return f.writeFileErr
	}
	f.files[path] = string(content)
	return nil
}

func (f *fakeHost) Close() error { return nil }

func (f *fakeHost) Run(ctx context.Context, command string, stdout, stderr io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)

	if strings.Contains(command, "exec -T forgejo true") {
		if f.execFailures > 0 {
			f.execFailures--
			return errors.New("container not ready")
		}
		return nil
	}
	if strings.Contains(command, "admin user create") {
		return f.adminCreateErr
	}
	return nil
}

func (f *fakeHost) CheckHost(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "docker version")
	return f.checkHostErr
}

func testBundle() *bundle.Bundle {
	return &bundle.Bundle{
		Manifest: *bundle.NewManifest("example.com", map[string]string{
			"forgejo": "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("a", 64),
		}, bundle.DriverConfig{
			Keystore: bundle.DriverRef{Driver: "file", Config: map[string]any{"path": "testdata/keys"}},
			Blob:     bundle.DriverRef{Driver: "local", Config: map[string]any{"path": "testdata/blobs"}},
		}),
		Compose: map[string][]byte{
			"docker-compose.yml": []byte("services:\n  forgejo:\n    image: x\n"),
		},
	}
}

func drain(job *events.Job) []events.Event {
	ch, cancel := job.Subscribe()
	defer cancel()
	var out []events.Event
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func TestUpSucceeds(t *testing.T) {
	host := newFakeHost()
	job := events.NewJob()

	err := Up(context.Background(), job, host, testBundle(), Options{RemoteDir: "/opt/farrier"})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	evs := drain(job)
	if len(evs) == 0 {
		t.Fatal("Up: emitted no events")
	}
	last := evs[len(evs)-1]
	if last.State != events.StateSucceeded || last.Step != "" {
		t.Errorf("last event = %+v, want a job-terminal success", last)
	}

	appINI, ok := host.files["/opt/farrier/forge/app.ini"]
	if !ok {
		t.Fatalf("app.ini not shipped, wrote: %v", keysOf(host.files))
	}
	if !strings.Contains(appINI, "INSTALL_LOCK = true") {
		t.Errorf("shipped app.ini missing INSTALL_LOCK: %s", appINI)
	}

	var sawCheckHost, sawComposeUp, sawExecReady, sawAdminCreate bool
	for _, cmd := range host.commands {
		switch {
		case strings.Contains(cmd, "docker version"):
			sawCheckHost = true
		case strings.Contains(cmd, "docker compose up -d"):
			sawComposeUp = true
			if !strings.Contains(cmd, "COMPOSE_PROJECT_NAME=farrier") {
				t.Errorf("compose up command missing project name: %q", cmd)
			}
		case strings.Contains(cmd, "exec -T forgejo true"):
			sawExecReady = true
		case strings.Contains(cmd, "admin user create"):
			sawAdminCreate = true
			if !strings.Contains(cmd, "COMPOSE_PROJECT_NAME=farrier") {
				t.Errorf("admin create command missing project name: %q", cmd)
			}
		}
	}
	if !sawCheckHost || !sawComposeUp || !sawExecReady || !sawAdminCreate {
		t.Fatalf("missing a step: checkHost=%v composeUp=%v execReady=%v adminCreate=%v (commands: %v)",
			sawCheckHost, sawComposeUp, sawExecReady, sawAdminCreate, host.commands)
	}
}

func TestUpRetriesUntilForgejoReady(t *testing.T) {
	host := newFakeHost()
	host.execFailures = 2
	job := events.NewJob()

	if err := Up(context.Background(), job, host, testBundle(), Options{RemoteDir: "/opt/farrier"}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	count := 0
	for _, cmd := range host.commands {
		if strings.Contains(cmd, "exec -T forgejo true") {
			count++
		}
	}
	if count != 3 {
		t.Errorf("readiness probes = %d, want 3 (2 failures + 1 success)", count)
	}
}

func TestUpFailsWhenDockerUnreachable(t *testing.T) {
	host := newFakeHost()
	host.checkHostErr = errors.New("no docker")
	job := events.NewJob()

	err := Up(context.Background(), job, host, testBundle(), Options{RemoteDir: "/opt/farrier"})
	if err == nil {
		t.Fatal("Up: want error when Docker is unreachable, got nil")
	}

	evs := drain(job)
	last := evs[len(evs)-1]
	if last.State != events.StateFailed || last.Step != "" {
		t.Errorf("last event = %+v, want a job-terminal failure", last)
	}
	if len(host.commands) != 1 || host.commands[0] != "docker version" {
		t.Errorf("commands past the host check: %v, want only the check itself", host.commands)
	}
}

func TestUpFailsWhenAdminCreateFails(t *testing.T) {
	host := newFakeHost()
	host.adminCreateErr = errors.New("create failed")
	job := events.NewJob()

	if err := Up(context.Background(), job, host, testBundle(), Options{RemoteDir: "/opt/farrier"}); err == nil {
		t.Fatal("Up: want error when admin bootstrap fails, got nil")
	}
}

func TestUpRejectsNilBundle(t *testing.T) {
	job := events.NewJob()
	if err := Up(context.Background(), job, newFakeHost(), nil, Options{RemoteDir: "/opt/farrier"}); err == nil {
		t.Fatal("Up: want error for nil bundle, got nil")
	}
}

func TestUpRejectsEmptyRemoteDir(t *testing.T) {
	job := events.NewJob()
	if err := Up(context.Background(), job, newFakeHost(), testBundle(), Options{}); err == nil {
		t.Fatal("Up: want error for empty remote directory, got nil")
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
