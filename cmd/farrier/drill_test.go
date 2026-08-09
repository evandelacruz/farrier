package main

import (
	"testing"
)

// TestDrillIsARegisteredCommand pins `drill` into the CLI's command table
// (spec.md "CLI commands" lists it as one of the ten).
func TestDrillIsARegisteredCommand(t *testing.T) {
	if commands["drill"] == nil {
		t.Fatal("commands has no drill entry")
	}
}

// TestRunDrillRequiresBundleTargetAndFrom asserts the flag contract before
// anything is dialed or loaded: a usage error exits 2, the same code every
// other command's missing-flag path returns.
func TestRunDrillRequiresBundleTargetAndFrom(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no flags", nil},
		{"missing bundle", []string{"-target", "ssh://user@host", "-from", "/tmp/src"}},
		{"missing target", []string{"-bundle", "/tmp/bundle", "-from", "/tmp/src"}},
		{"missing from", []string{"-bundle", "/tmp/bundle", "-target", "ssh://user@host"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code := runDrill(tc.args); code != 2 {
				t.Errorf("runDrill(%v) = %d, want 2", tc.args, code)
			}
		})
	}
}

// TestRunDrillRejectsASnapshotFlag pins DRIL-001's "the most recent
// backup": there is deliberately no way to drill an older snapshot, so an
// unknown -snapshot flag is a usage error rather than a silently ignored
// one.
func TestRunDrillRejectsASnapshotFlag(t *testing.T) {
	args := []string{"-bundle", "/tmp/bundle", "-target", "ssh://user@host", "-from", "/tmp/src", "-snapshot", "20260101T000000Z.age"}
	if code := runDrill(args); code != 2 {
		t.Errorf("runDrill with -snapshot = %d, want 2 (unknown flag)", code)
	}
}
