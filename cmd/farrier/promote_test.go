package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/backup"
)

// writeSnapshot puts a snapshot object at dir under key, with its mtime set
// to modified — the same "real local destination, explicit Chtimes" pattern
// destination_test.go uses to control Object.Modified for age assertions.
func writeSnapshot(t *testing.T, dir, key string, modified time.Time) {
	t.Helper()
	adapter, err := backup.OpenDestination(dir)
	if err != nil {
		t.Fatalf("OpenDestination: %v", err)
	}
	if err := adapter.Put(context.Background(), key, bytes.NewReader([]byte("snapshot")), 8); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := os.Chtimes(filepath.Join(dir, key), modified, modified); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func TestConfirmPromoteDisplaysAgeAndSkipsPromptWithYes(t *testing.T) {
	dir := t.TempDir()
	key := backup.SnapshotKey(time.Now().Add(-2 * time.Hour))
	writeSnapshot(t, dir, key, time.Now().Add(-2*time.Hour))

	var out bytes.Buffer
	confirmed := confirmPromote(context.Background(), strings.NewReader(""), &out, dir, "", true)

	if !confirmed {
		t.Fatal("confirmPromote with skipPrompt=true: want confirmed, got false")
	}
	if !strings.Contains(out.String(), key) {
		t.Errorf("output %q does not mention resolved snapshot key %q", out.String(), key)
	}
	if !strings.Contains(out.String(), "ago") {
		t.Errorf("output %q does not display an age", out.String())
	}
}

func TestConfirmPromoteAcceptsTypedYes(t *testing.T) {
	dir := t.TempDir()
	key := backup.SnapshotKey(time.Now().Add(-time.Hour))
	writeSnapshot(t, dir, key, time.Now().Add(-time.Hour))

	var out bytes.Buffer
	confirmed := confirmPromote(context.Background(), strings.NewReader("yes\n"), &out, dir, "", false)
	if !confirmed {
		t.Fatal("confirmPromote: want confirmed for typed \"yes\", got false")
	}
}

func TestConfirmPromoteRejectsAnythingElse(t *testing.T) {
	dir := t.TempDir()
	key := backup.SnapshotKey(time.Now().Add(-time.Hour))
	writeSnapshot(t, dir, key, time.Now().Add(-time.Hour))

	cases := []string{"", "no\n", "y\n", "Yes please\n"}
	for _, in := range cases {
		var out bytes.Buffer
		confirmed := confirmPromote(context.Background(), strings.NewReader(in), &out, dir, "", false)
		if confirmed {
			t.Errorf("confirmPromote(%q): want not confirmed, got confirmed", in)
		}
	}
}

func TestConfirmPromoteFailsOnUnresolvableSnapshot(t *testing.T) {
	dir := t.TempDir() // no snapshots written

	var out bytes.Buffer
	confirmed := confirmPromote(context.Background(), strings.NewReader("yes\n"), &out, dir, "", false)
	if confirmed {
		t.Fatal("confirmPromote: want not confirmed when the snapshot can't be resolved, got confirmed")
	}
}
