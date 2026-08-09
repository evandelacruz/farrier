package restore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/backup"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
)

func TestGitComponentPairsGroupsByName(t *testing.T) {
	manifest := &backup.Manifest{Components: []backup.Component{
		{Kind: bundle.StateKindGit, Name: "acme/widgets", Path: "repos/acme/widgets.tar"},
		{Kind: bundle.StateKindGit, Name: "acme/widgets", Path: "repos/acme/widgets.refs.tar"},
		{Kind: bundle.StateKindGit, Name: "acme/gadgets", Path: "repos/acme/gadgets.refs.tar"},
		{Kind: bundle.StateKindGit, Name: "acme/gadgets", Path: "repos/acme/gadgets.tar"},
		{Kind: bundle.StateKindDatabase, Name: "db.sqlite", Path: "db.sqlite"},
	}}

	pairs, names, err := gitComponentPairs(manifest)
	if err != nil {
		t.Fatalf("gitComponentPairs: %v", err)
	}
	if len(names) != 2 || names[0] != "acme/gadgets" || names[1] != "acme/widgets" {
		t.Fatalf("names = %v, want sorted [acme/gadgets acme/widgets]", names)
	}
	if pairs["acme/widgets"].objectsPath != "repos/acme/widgets.tar" {
		t.Errorf("widgets objectsPath = %q", pairs["acme/widgets"].objectsPath)
	}
	if pairs["acme/widgets"].refsPath != "repos/acme/widgets.refs.tar" {
		t.Errorf("widgets refsPath = %q", pairs["acme/widgets"].refsPath)
	}
}

func TestGitComponentPairsRefusesTornSnapshot(t *testing.T) {
	tests := []struct {
		name       string
		components []backup.Component
		wantSubstr string
	}{
		{
			name: "missing refs",
			components: []backup.Component{
				{Kind: bundle.StateKindGit, Name: "acme/widgets", Path: "repos/acme/widgets.tar"},
			},
			wantSubstr: "missing the ref archive",
		},
		{
			name: "missing objects",
			components: []backup.Component{
				{Kind: bundle.StateKindGit, Name: "acme/widgets", Path: "repos/acme/widgets.refs.tar"},
			},
			wantSubstr: "missing the object archive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := gitComponentPairs(&backup.Manifest{Components: tt.components})
			if err == nil {
				t.Fatal("gitComponentPairs: want error for a torn snapshot, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("error %q does not contain %q", err, tt.wantSubstr)
			}
			if !strings.Contains(err.Error(), "acme/widgets") {
				t.Errorf("error %q does not name the affected repository", err)
			}
		})
	}
}

func TestDatabaseComponentRequiresExactlyOne(t *testing.T) {
	if _, err := databaseComponent(&backup.Manifest{}); err == nil {
		t.Fatal("databaseComponent: want error when no database component is present, got nil")
	}

	two := &backup.Manifest{Components: []backup.Component{
		{Kind: bundle.StateKindDatabase, Name: "a", Path: "a"},
		{Kind: bundle.StateKindDatabase, Name: "b", Path: "b"},
	}}
	if _, err := databaseComponent(two); err == nil {
		t.Fatal("databaseComponent: want error for two database components, got nil")
	}

	one := &backup.Manifest{Components: []backup.Component{
		{Kind: bundle.StateKindDatabase, Name: "db.sqlite", Path: "db.sqlite"},
	}}
	c, err := databaseComponent(one)
	if err != nil {
		t.Fatalf("databaseComponent: %v", err)
	}
	if c.Name != "db.sqlite" {
		t.Errorf("Name = %q, want db.sqlite", c.Name)
	}
}

func TestDatabaseRelPathMatchesForgePaths(t *testing.T) {
	got := databaseRelPath()
	want := strings.TrimPrefix(forge.DatabasePath, forge.DataPath+"/")
	if got != want {
		t.Errorf("databaseRelPath() = %q, want %q", got, want)
	}
	if filepath.Join(forge.DataPath, got) != forge.DatabasePath {
		t.Errorf("DataPath+databaseRelPath() = %q, want %q", filepath.Join(forge.DataPath, got), forge.DatabasePath)
	}
}

// TestPlaceOneRepoAppliesObjectsBeforeRefs proves the extraction order
// capture.go's own doc comments require: the object archive first, then the
// ref archive on top, so the final ref state matches the moment BKUP-002's
// push hold was held rather than whatever the object archive's later
// capture happened to see.
func TestPlaceOneRepoAppliesObjectsBeforeRefs(t *testing.T) {
	dir := t.TempDir()
	objectsPath := filepath.Join(dir, "widgets.tar")
	refsPath := filepath.Join(dir, "widgets.refs.tar")
	if err := os.WriteFile(objectsPath, []byte("objects-content"), 0o600); err != nil {
		t.Fatalf("write objects: %v", err)
	}
	if err := os.WriteFile(refsPath, []byte("refs-content"), 0o600); err != nil {
		t.Fatalf("write refs: %v", err)
	}

	host := newFakeHost()
	err := placeOneRepo(context.Background(), host, dir, "/opt/farrier/state/git", "acme/widgets", gitPair{
		objectsPath: "widgets.tar",
		refsPath:    "widgets.refs.tar",
	})
	if err != nil {
		t.Fatalf("placeOneRepo: %v", err)
	}

	target := "/opt/farrier/state/git/acme/widgets.git"
	extracts := host.commandsContaining("tar -C '" + target + "' -xf -")
	if len(extracts) != 2 {
		t.Fatalf("got %d extract commands, want 2: %v", len(extracts), host.commands)
	}
	if string(host.stdins[len(host.stdins)-2].data) != "objects-content" {
		t.Errorf("first extraction streamed %q, want the object archive", host.stdins[len(host.stdins)-2].data)
	}
	if string(host.stdins[len(host.stdins)-1].data) != "refs-content" {
		t.Errorf("second extraction streamed %q, want the ref archive", host.stdins[len(host.stdins)-1].data)
	}
}

func TestPlaceStateFailsLoudlyOnTornGitSnapshot(t *testing.T) {
	dir := t.TempDir()
	manifest := &backup.Manifest{Components: []backup.Component{
		{Kind: bundle.StateKindGit, Name: "acme/widgets", Path: "repos/acme/widgets.tar"},
		{Kind: bundle.StateKindDatabase, Name: "db.sqlite", Path: "db.sqlite"},
	}}

	host := newFakeHost()
	job := events.NewJob()
	err := placeState(context.Background(), job, dir, manifest, Options{RemoteDir: "/opt/farrier", Host: host})
	if err == nil {
		t.Fatal("placeState: want error for a torn git snapshot, got nil")
	}
	if len(host.commands) != 0 {
		t.Errorf("host was touched despite the snapshot being torn: %v", host.commands)
	}
}
