package acme

import (
	"strings"
	"testing"
)

// TestResolveDirectoryURL covers the three answers an operator's ACME
// server choice can have — the default, the shorthand, and a URL of their
// own — and the refusals that keep a bad one from reaching a host.
func TestResolveDirectoryURL(t *testing.T) {
	t.Run("empty is Let's Encrypt production", func(t *testing.T) {
		got, err := ResolveDirectoryURL("")
		if err != nil {
			t.Fatalf("ResolveDirectoryURL(\"\"): %v", err)
		}
		if got != ProductionDirectoryURL {
			t.Errorf("ResolveDirectoryURL(\"\") = %q, want %q", got, ProductionDirectoryURL)
		}
	})

	t.Run("the shorthand expands to Let's Encrypt staging", func(t *testing.T) {
		for _, in := range []string{StagingShorthand, "  " + StagingShorthand + "  "} {
			got, err := ResolveDirectoryURL(in)
			if err != nil {
				t.Fatalf("ResolveDirectoryURL(%q): %v", in, err)
			}
			if got != StagingDirectoryURL {
				t.Errorf("ResolveDirectoryURL(%q) = %q, want %q", in, got, StagingDirectoryURL)
			}
		}
	})

	t.Run("an explicit URL is carried through unchanged", func(t *testing.T) {
		// An internal ACME CA (step-ca and its kind) is the case the URL
		// form exists for beyond staging, so both schemes are covered.
		for _, in := range []string{
			"https://ca.internal.example.com/acme/acme/directory",
			"http://step-ca.lan:9000/acme/acme/directory",
			StagingDirectoryURL,
		} {
			got, err := ResolveDirectoryURL(in)
			if err != nil {
				t.Fatalf("ResolveDirectoryURL(%q): %v", in, err)
			}
			if got != in {
				t.Errorf("ResolveDirectoryURL(%q) = %q, want it unchanged", in, got)
			}
		}
	})

	t.Run("a malformed value is refused", func(t *testing.T) {
		for _, in := range []string{
			"production", // a shorthand that does not exist
			"acme-staging-v02.api.letsencrypt.org/directory", // no scheme
			"ftp://ca.example.com/directory",                 // not an ACME transport
			"https://",                                       // no host
			"://nonsense",
		} {
			if got, err := ResolveDirectoryURL(in); err == nil {
				t.Errorf("ResolveDirectoryURL(%q) = %q, want an error", in, got)
			}
		}
	})

	t.Run("a refusal names the value and the shorthand", func(t *testing.T) {
		_, err := ResolveDirectoryURL("staging.letsencrypt.org")
		if err == nil {
			t.Fatal("want an error for a value with no scheme")
		}
		if !strings.Contains(err.Error(), "staging.letsencrypt.org") {
			t.Errorf("error %q does not name the value the operator gave", err)
		}
		if !strings.Contains(err.Error(), StagingShorthand) {
			t.Errorf("error %q does not offer the shorthand", err)
		}
	})
}

// TestConfigDirectoryURLDefault covers the reader side of the same
// decision: a Config that names no CA — which is what a manifest written
// before the bundle recorded one produces — reaches Let's Encrypt
// production, and one that names a CA reaches exactly that.
func TestConfigDirectoryURLDefault(t *testing.T) {
	if got := (Config{}).directoryURL(); got != ProductionDirectoryURL {
		t.Errorf("empty Config directory = %q, want %q", got, ProductionDirectoryURL)
	}
	if got := (Config{DirectoryURL: "  "}).directoryURL(); got != ProductionDirectoryURL {
		t.Errorf("blank Config directory = %q, want %q", got, ProductionDirectoryURL)
	}
	if got := (Config{DirectoryURL: StagingDirectoryURL}).directoryURL(); got != StagingDirectoryURL {
		t.Errorf("staging Config directory = %q, want it unchanged", got)
	}
}

// TestLetsEncryptDirectoriesDiffer guards the pair of constants the whole
// feature rests on: staging and production must be two different servers,
// or rehearsing against staging would spend production's rate limits after
// all.
func TestLetsEncryptDirectoriesDiffer(t *testing.T) {
	if StagingDirectoryURL == ProductionDirectoryURL {
		t.Fatal("staging and production resolve to the same ACME directory")
	}
	if !strings.Contains(StagingDirectoryURL, "staging") {
		t.Errorf("staging directory %q does not look like a staging endpoint", StagingDirectoryURL)
	}
}
