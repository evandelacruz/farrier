package main

import "testing"

func TestRunImportRequiresBundle(t *testing.T) {
	code := withSilencedStderr(t, func() int {
		return runImport([]string{"-forge-token", "t", "-source", "https://github.com/acme/widgets.git", "-source-token", "s"})
	})
	if code != 2 {
		t.Errorf("runImport without -bundle: exit code = %d, want 2", code)
	}
}

func TestRunImportRequiresForgeToken(t *testing.T) {
	code := withSilencedStderr(t, func() int {
		return runImport([]string{"-bundle", ".", "-source", "https://github.com/acme/widgets.git", "-source-token", "s"})
	})
	if code != 2 {
		t.Errorf("runImport without -forge-token: exit code = %d, want 2", code)
	}
}

func TestRunImportRequiresSource(t *testing.T) {
	code := withSilencedStderr(t, func() int {
		return runImport([]string{"-bundle", ".", "-forge-token", "t", "-source-token", "s"})
	})
	if code != 2 {
		t.Errorf("runImport without -source: exit code = %d, want 2", code)
	}
}

func TestRunImportRequiresSourceToken(t *testing.T) {
	code := withSilencedStderr(t, func() int {
		return runImport([]string{"-bundle", ".", "-forge-token", "t", "-source", "https://github.com/acme/widgets.git"})
	})
	if code != 2 {
		t.Errorf("runImport without -source-token: exit code = %d, want 2", code)
	}
}

func TestRunImportFailsToLoadMissingBundle(t *testing.T) {
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-bundle", "/nonexistent/bundle/dir",
			"-forge-token", "t",
			"-source", "https://github.com/acme/widgets.git",
			"-source-token", "s",
		})
	})
	if code != 1 {
		t.Errorf("runImport with a missing bundle dir: exit code = %d, want 1", code)
	}
}
