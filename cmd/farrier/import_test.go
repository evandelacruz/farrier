package main

import "testing"

func TestRunImportRequiresTarget(t *testing.T) {
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-target-token", "t", "-source", "https://github.com/acme/widgets",
			"-source-token", "s", "-owner", "acme",
		})
	})
	if code != 2 {
		t.Errorf("runImport without -target: exit code = %d, want 2", code)
	}
}

func TestRunImportRequiresTargetToken(t *testing.T) {
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-target", "https://git.example.com", "-source", "https://github.com/acme/widgets",
			"-source-token", "s", "-owner", "acme",
		})
	})
	if code != 2 {
		t.Errorf("runImport without -target-token: exit code = %d, want 2", code)
	}
}

func TestRunImportRequiresSource(t *testing.T) {
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-target", "https://git.example.com", "-target-token", "t",
			"-source-token", "s", "-owner", "acme",
		})
	})
	if code != 2 {
		t.Errorf("runImport without -source: exit code = %d, want 2", code)
	}
}

func TestRunImportRequiresSourceToken(t *testing.T) {
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-target", "https://git.example.com", "-target-token", "t",
			"-source", "https://github.com/acme/widgets", "-owner", "acme",
		})
	})
	if code != 2 {
		t.Errorf("runImport without -source-token: exit code = %d, want 2", code)
	}
}

func TestRunImportRequiresOwner(t *testing.T) {
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-target", "https://git.example.com", "-target-token", "t",
			"-source", "https://github.com/acme/widgets", "-source-token", "s",
		})
	})
	if code != 2 {
		t.Errorf("runImport without -owner: exit code = %d, want 2", code)
	}
}

func TestRunImportFailsAgainstUnreachableTarget(t *testing.T) {
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-target", "https://127.0.0.1:1", "-target-token", "t",
			"-source", "https://github.com/acme/widgets", "-source-token", "s",
			"-owner", "acme",
		})
	})
	if code != 1 {
		t.Errorf("runImport against an unreachable target: exit code = %d, want 1", code)
	}
}
