package main

import (
	"testing"
)

func TestRunPublishRequiresTargetToken(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "")
	code := withSilencedStderr(t, func() int {
		return runPublish([]string{"-dir", t.TempDir()})
	})
	if code != 2 {
		t.Errorf("runPublish without FARRIER_TARGET_TOKEN: exit code = %d, want 2", code)
	}
}

// The bundle is what carries the instance's address, so a folder that has
// never been through `init` fails before anything is inspected or created.
func TestRunPublishRequiresABundle(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	code := withSilencedStderr(t, func() int {
		return runPublish([]string{"-dir", t.TempDir()})
	})
	if code != 1 {
		t.Errorf("runPublish without a bundle: exit code = %d, want 1", code)
	}
}

// A nameless instance is reached at an address rather than a domain
// (UP-006), so the flag that carries one is part of the command's surface.
func TestRunPublishAcceptsAnAddress(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	code := withSilencedStderr(t, func() int {
		return runPublish([]string{"-dir", t.TempDir(), "-address", "192.168.1.5"})
	})
	// 1, not 2: the flag parsed, and the run failed on the missing bundle.
	if code != 1 {
		t.Errorf("runPublish with -address: exit code = %d, want 1", code)
	}
}

func TestPublishIsARegisteredCommand(t *testing.T) {
	if _, ok := commands["publish"]; !ok {
		t.Error("publish is not in the command table")
	}
}
