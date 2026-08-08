package main

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestRunImportRejectsSourceAndFileTogether(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	t.Setenv("FARRIER_SOURCE_TOKEN", "s")
	manifest := writeManifest(t, `repos:
  - source: https://github.com/acme/widgets
`)
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-target", "https://git.example.com",
			"-source", "https://github.com/acme/widgets",
			"-file", manifest,
			"-owner", "acme",
		})
	})
	if code != 2 {
		t.Errorf("runImport with both -source and -file: exit code = %d, want 2", code)
	}
}

func TestRunImportRequiresSourceOrFile(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	t.Setenv("FARRIER_SOURCE_TOKEN", "s")
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-target", "https://git.example.com",
			"-owner", "acme",
		})
	})
	if code != 2 {
		t.Errorf("runImport with neither -source nor -file: exit code = %d, want 2", code)
	}
}

func TestRunImportBatchFailsAgainstUnreachableTarget(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	t.Setenv("FARRIER_SOURCE_TOKEN", "s")
	manifest := writeManifest(t, `repos:
  - source: https://github.com/acme/widgets
  - source: https://github.com/acme/gadgets
`)
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-target", "https://127.0.0.1:1",
			"-file", manifest,
			"-owner", "acme",
		})
	})
	if code != 1 {
		t.Errorf("runImport -file against an unreachable target: exit code = %d, want 1", code)
	}
}

func TestRunImportBatchRequiresOwnerWhenManifestHasNone(t *testing.T) {
	t.Setenv("FARRIER_TARGET_TOKEN", "t")
	t.Setenv("FARRIER_SOURCE_TOKEN", "s")
	manifest := writeManifest(t, `repos:
  - source: https://github.com/acme/widgets
`)
	code := withSilencedStderr(t, func() int {
		return runImport([]string{
			"-target", "https://git.example.com",
			"-file", manifest,
		})
	})
	if code != 2 {
		t.Errorf("runImport -file with no owner anywhere: exit code = %d, want 2", code)
	}
}

func writeManifest(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "import.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}
