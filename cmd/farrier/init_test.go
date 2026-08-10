package main

import (
	"os"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/initialize"
)

func TestKeyValueFlagSetRejectsMissingEquals(t *testing.T) {
	var f keyValueFlag
	if err := f.Set("no-equals-sign"); err == nil {
		t.Fatal("Set: want error for value with no '=', got nil")
	}
}

func TestKeyValueFlagAsAnyAndAsStrings(t *testing.T) {
	var f keyValueFlag
	if err := f.Set("path=/tmp/keys"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := f.Set("mode=strict"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	asAny := f.asAny()
	if asAny["path"] != "/tmp/keys" || asAny["mode"] != "strict" {
		t.Errorf("asAny() = %+v", asAny)
	}

	asStrings := f.asStrings()
	if asStrings["path"] != "/tmp/keys" || asStrings["mode"] != "strict" {
		t.Errorf("asStrings() = %+v", asStrings)
	}
}

func TestKeyValueFlagEmptyYieldsNilMaps(t *testing.T) {
	var f keyValueFlag
	if f.asAny() != nil {
		t.Errorf("asAny() on empty flag = %+v, want nil", f.asAny())
	}
	if f.asStrings() != nil {
		t.Errorf("asStrings() on empty flag = %+v, want nil", f.asStrings())
	}
}

func TestKeyValueFlagValueAllowsEmptyValue(t *testing.T) {
	var f keyValueFlag
	if err := f.Set("key="); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := f.asStrings()["key"]; got != "" {
		t.Errorf("asStrings()[\"key\"] = %q, want empty string", got)
	}
}

func TestRunInitRequiresDomain(t *testing.T) {
	code := withSilencedStderr(t, func() int {
		return runInit([]string{"-keystore-driver", "file", "-blob-driver", "local", "-acme-dns-provider", "manual"})
	})
	if code != 2 {
		t.Errorf("runInit without -domain: exit code = %d, want 2", code)
	}
}

func TestRunInitRequiresKeystoreDriver(t *testing.T) {
	code := withSilencedStderr(t, func() int {
		return runInit([]string{"-domain", "example.com", "-blob-driver", "local", "-acme-dns-provider", "manual"})
	})
	if code != 2 {
		t.Errorf("runInit without -keystore-driver: exit code = %d, want 2", code)
	}
}

func TestRunInitRequiresBlobDriver(t *testing.T) {
	code := withSilencedStderr(t, func() int {
		return runInit([]string{"-domain", "example.com", "-keystore-driver", "file", "-acme-dns-provider", "manual"})
	})
	if code != 2 {
		t.Errorf("runInit without -blob-driver: exit code = %d, want 2", code)
	}
}

func TestRunInitRequiresACMEDNSProvider(t *testing.T) {
	code := withSilencedStderr(t, func() int {
		return runInit([]string{"-domain", "example.com", "-keystore-driver", "file", "-blob-driver", "local"})
	})
	if code != 2 {
		t.Errorf("runInit without -acme-dns-provider: exit code = %d, want 2", code)
	}
}

func TestRunInitRejectsAnEmptyProject(t *testing.T) {
	code := withSilencedStderr(t, func() int {
		return runInit([]string{"-domain", "example.com", "-project", "", "-keystore-driver", "file", "-blob-driver", "local", "-acme-dns-provider", "manual"})
	})
	if code != 2 {
		t.Errorf("runInit with an empty -project: exit code = %d, want 2", code)
	}
}

// INIT-001: `cd my-project && farrier init` targets the working directory
// and lands the bundle in .farrier/ inside it, with no -dir to supply.
func TestParseInitFlagsDefaultsToTheWorkingDirectoryProject(t *testing.T) {
	params, code := parseInitFlags([]string{"-domain", "example.com", "-keystore-driver", "file", "-blob-driver", "local", "-acme-dns-provider", "manual"})
	if code != 0 {
		t.Fatalf("parseInitFlags: exit code = %d, want 0", code)
	}
	if params.Project != "." {
		t.Errorf("Project = %q, want \".\"", params.Project)
	}
	if params.Dir != "" {
		t.Errorf("Dir = %q, want it left empty so the core picks the default", params.Dir)
	}
	if got := initialize.BundleDir(params); got != bundle.DirName {
		t.Errorf("resolved bundle dir = %q, want %q", got, bundle.DirName)
	}
}

// INIT-001: -dir moves the bundle out of the project, for an instance that
// serves several of them.
func TestParseInitFlagsHonorsAnExplicitDir(t *testing.T) {
	params, code := parseInitFlags([]string{"-domain", "example.com", "-project", "/srv/my-project", "-dir", "/srv/forge", "-keystore-driver", "file", "-blob-driver", "local", "-acme-dns-provider", "manual"})
	if code != 0 {
		t.Fatalf("parseInitFlags: exit code = %d, want 0", code)
	}
	if params.Project != "/srv/my-project" {
		t.Errorf("Project = %q", params.Project)
	}
	if got := initialize.BundleDir(params); got != "/srv/forge" {
		t.Errorf("resolved bundle dir = %q, want /srv/forge", got)
	}
}

// withSilencedStderr redirects os.Stderr to /dev/null for the duration of
// fn, so tests exercising farrier's usage-error paths don't spam test
// output with expected diagnostics.
func withSilencedStderr(t *testing.T, fn func() int) int {
	t.Helper()
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer null.Close()

	orig := os.Stderr
	os.Stderr = null
	defer func() { os.Stderr = orig }()

	return fn()
}
