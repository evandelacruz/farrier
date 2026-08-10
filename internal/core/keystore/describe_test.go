package keystore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileDriverDescribesTheFileAKeyLivesIn(t *testing.T) {
	dir := t.TempDir()
	driver := FileDriver{Path: dir}

	got := driver.DescribeTarget("age_backup_key")
	want := filepath.Join(dir, "age_backup_key")
	if got != want {
		t.Errorf("DescribeTarget = %q, want %q", got, want)
	}
}

func TestFileDriverDescribesNothingForAKeyItWouldRefuse(t *testing.T) {
	driver := FileDriver{Path: t.TempDir()}

	for _, keyName := range []string{"", "   ", "../escape"} {
		if got := driver.DescribeTarget(keyName); got != "" {
			t.Errorf("DescribeTarget(%q) = %q, want no description for a key name the driver would refuse", keyName, got)
		}
	}
}

// TestTargetSurvivesTheRotationGuard is the case INIT-006 actually runs:
// every caller gets its driver from New, which wraps a writer in the
// rotation guard, so a location report that only worked on a bare driver
// would report nothing in production.
func TestTargetSurvivesTheRotationGuard(t *testing.T) {
	dir := t.TempDir()
	driver, err := New("file", map[string]any{"path": dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := Target(driver, "ssh_host_key")
	want := filepath.Join(dir, "ssh_host_key")
	if got != want {
		t.Errorf("Target = %q, want %q", got, want)
	}
}

// TestTargetIsEmptyForADriverThatCannotSay covers the deliberate
// half-answer: the command driver hands storage to an operator's command
// and an out-of-tree exec driver speaks a protocol with no describe
// method, so both report nothing rather than a guess, and a caller names
// the driver alone.
func TestTargetIsEmptyForADriverThatCannotSay(t *testing.T) {
	cases := map[string]struct {
		driver string
		config map[string]any
	}{
		"command":   {driver: "command", config: map[string]any{"command": "op read op://vault/item"}},
		"exec":      {driver: "vault", config: map[string]any{"path": "/usr/local/bin/farrier-vault"}},
		"bare file": {},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var driver Driver = stubDriver{}
			if tc.driver != "" {
				built, err := New(tc.driver, tc.config)
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				driver = built
			}
			if got := Target(driver, "age_backup_key"); got != "" {
				t.Errorf("Target = %q, want empty for a driver that does not describe its target", got)
			}
		})
	}
}

// stubDriver is a Driver that implements neither Writer nor Describer —
// the shape an out-of-tree driver author is free to ship.
type stubDriver struct{}

func (stubDriver) Resolve(context.Context, string) (Secret, error) {
	return Secret{}, ErrNotFound
}

// TestDescribeTargetNeverRevealsKeyMaterial pins KEY-003 on the reporting
// path: a description names a destination, and the value stored there
// never appears in it.
func TestDescribeTargetNeverRevealsKeyMaterial(t *testing.T) {
	dir := t.TempDir()
	driver, err := New("file", map[string]any{"path": dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const value = "AGE-SECRET-KEY-1EXAMPLEEXAMPLEEXAMPLE"
	if err := driver.(Writer).Store(context.Background(), "age_backup_key", NewSecret(value)); err != nil {
		t.Fatalf("Store: %v", err)
	}

	if got := Target(driver, "age_backup_key"); strings.Contains(got, value) {
		t.Errorf("Target = %q, want a destination that does not contain the stored value", got)
	}
}
