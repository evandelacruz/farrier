package orchestrate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
)

// fakeTransport records every Output and WriteFile call in order, so tests can
// assert on the exact sequence Converge drives, without a real host.
type fakeTransport struct {
	commands []string
	files    map[string]string
	runErr   error
	writeErr error
	closed   bool
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{files: make(map[string]string)}
}

func (f *fakeTransport) Output(ctx context.Context, command string) ([]byte, error) {
	f.commands = append(f.commands, command)
	if f.runErr != nil {
		return nil, f.runErr
	}
	return nil, nil
}

func (f *fakeTransport) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.files[path] = string(content)
	return nil
}

func (f *fakeTransport) Close() error {
	f.closed = true
	return nil
}

func testBundle() *bundle.Bundle {
	return &bundle.Bundle{
		Manifest: *testManifest(),
		Compose: map[string][]byte{
			"docker-compose.yml": []byte("services: {}\n"),
		},
	}
}

func TestConvergeShipsFilesAndRunsComposeUp(t *testing.T) {
	transport := newFakeTransport()
	b := testBundle()

	if err := Converge(context.Background(), transport, "/opt/farrier", b); err != nil {
		t.Fatalf("Converge: %v", err)
	}

	wantContent := "services: {}\n"
	gotContent, ok := transport.files["/opt/farrier/compose.tmp/docker-compose.yml"]
	if !ok {
		t.Fatalf("compose file not shipped, wrote: %v", transport.files)
	}
	if gotContent != wantContent {
		t.Errorf("shipped content = %q, want %q", gotContent, wantContent)
	}

	// The last command must be the docker compose up invocation, naming
	// the shipped file and requesting orphan removal so drift is torn
	// down.
	last := transport.commands[len(transport.commands)-1]
	if !strings.Contains(last, "docker compose") || !strings.Contains(last, "up -d") || !strings.Contains(last, "--remove-orphans") {
		t.Errorf("final command = %q, want a docker compose up --remove-orphans", last)
	}
	if !strings.Contains(last, "compose/docker-compose.yml") {
		t.Errorf("final command = %q, want it to reference the shipped file", last)
	}

	// The staging directory must be swapped into place before compose up
	// runs, and cleared before staging begins.
	var sawStage, sawInstall, sawUp bool
	for _, cmd := range transport.commands {
		switch {
		case strings.Contains(cmd, "mkdir -p") && strings.Contains(cmd, "compose.tmp"):
			sawStage = true
			if sawInstall {
				t.Errorf("staged after install: %q", cmd)
			}
		case strings.Contains(cmd, "mv") && strings.Contains(cmd, "compose.tmp"):
			sawInstall = true
			if !sawStage {
				t.Errorf("installed before staging: %q", cmd)
			}
		case strings.Contains(cmd, "docker compose"):
			sawUp = true
			if !sawInstall {
				t.Errorf("compose up before install: %q", cmd)
			}
		}
	}
	if !sawStage || !sawInstall || !sawUp {
		t.Fatalf("missing a step: stage=%v install=%v up=%v (commands: %v)", sawStage, sawInstall, sawUp, transport.commands)
	}
}

func TestConvergeShipsFilesInSortedOrder(t *testing.T) {
	transport := newFakeTransport()
	b := &bundle.Bundle{
		Manifest: *testManifest(),
		Compose: map[string][]byte{
			"zzz.yml": []byte("z\n"),
			"aaa.yml": []byte("a\n"),
		},
	}

	if err := Converge(context.Background(), transport, "/opt/farrier", b); err != nil {
		t.Fatalf("Converge: %v", err)
	}

	var written []string
	for path := range transport.files {
		written = append(written, path)
	}
	sort.Strings(written)
	if len(written) != 2 {
		t.Fatalf("wrote %d files, want 2: %v", len(written), written)
	}

	up := transport.commands[len(transport.commands)-1]
	aIdx := strings.Index(up, "aaa.yml")
	zIdx := strings.Index(up, "zzz.yml")
	if aIdx == -1 || zIdx == -1 || aIdx > zIdx {
		t.Errorf("compose up command doesn't list files in sorted order: %q", up)
	}
}

func TestConvergeRejectsEmptyRemoteDir(t *testing.T) {
	if err := Converge(context.Background(), newFakeTransport(), "", testBundle()); err == nil {
		t.Fatal("Converge: want error for empty remote directory, got nil")
	}
}

func TestConvergeRejectsBundleWithNoCompose(t *testing.T) {
	b := &bundle.Bundle{Manifest: *testManifest()}
	if err := Converge(context.Background(), newFakeTransport(), "/opt/farrier", b); err == nil {
		t.Fatal("Converge: want error for bundle with no compose files, got nil")
	}
}

func TestConvergeStopsOnWriteFailure(t *testing.T) {
	transport := newFakeTransport()
	transport.writeErr = errors.New("disk full")

	err := Converge(context.Background(), transport, "/opt/farrier", testBundle())
	if err == nil {
		t.Fatal("Converge: want error when WriteFile fails, got nil")
	}
	for _, cmd := range transport.commands {
		if strings.Contains(cmd, "docker compose") {
			t.Errorf("docker compose ran despite a failed file write: %q", cmd)
		}
	}
}

func TestConvergeStopsOnRunFailure(t *testing.T) {
	transport := newFakeTransport()
	transport.runErr = fmt.Errorf("connection reset")

	if err := Converge(context.Background(), transport, "/opt/farrier", testBundle()); err == nil {
		t.Fatal("Converge: want error when Run fails, got nil")
	}
	if len(transport.files) != 0 {
		t.Errorf("files written despite staging failure: %v", transport.files)
	}
}
