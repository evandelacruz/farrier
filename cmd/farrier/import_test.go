package main

import "testing"

func TestRunImportRequiresTarget(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	t.Setenv("FARRIER_SOURCE_TOKEN", "s")
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-source", "https://github.com/acme/widgets", "-owner", "acme",
		})
	})
	if code != 2 {
		t.Errorf("runImport without -target: exit code = %d, want 2", code)
	}
}

func TestRunImportRequiresTargetToken(t *testing.T) {
	t.Setenv("FARRIER_SOURCE_TOKEN", "s")
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-target", "https://git.example.com", "-source", "https://github.com/acme/widgets",
			"-owner", "acme",
		})
	})
	if code != 2 {
		t.Errorf("runImport without FARRIER_TARGET_TOKEN: exit code = %d, want 2", code)
	}
}

func TestRunImportRequiresSource(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	t.Setenv("FARRIER_SOURCE_TOKEN", "s")
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-target", "https://git.example.com", "-owner", "acme",
		})
	})
	if code != 2 {
		t.Errorf("runImport without -source: exit code = %d, want 2", code)
	}
}

func TestRunImportRequiresSourceToken(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-target", "https://git.example.com",
			"-source", "https://github.com/acme/widgets", "-owner", "acme",
		})
	})
	if code != 2 {
		t.Errorf("runImport without FARRIER_SOURCE_TOKEN: exit code = %d, want 2", code)
	}
}

func TestRunImportRequiresOwner(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	t.Setenv("FARRIER_SOURCE_TOKEN", "s")
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-target", "https://git.example.com",
			"-source", "https://github.com/acme/widgets",
		})
	})
	if code != 2 {
		t.Errorf("runImport without -owner: exit code = %d, want 2", code)
	}
}

func TestRunImportFailsAgainstUnreachableTarget(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	t.Setenv("FARRIER_SOURCE_TOKEN", "s")
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-target", "https://127.0.0.1:1",
			"-source", "https://github.com/acme/widgets",
			"-owner", "acme",
		})
	})
	if code != 1 {
		t.Errorf("runImport against an unreachable target: exit code = %d, want 1", code)
	}
}
