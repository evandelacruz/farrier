package main

import "testing"

func TestUIIsARegisteredCommand(t *testing.T) {
	if _, ok := commands["ui"]; !ok {
		t.Fatal("commands has no \"ui\" entry (UI-001)")
	}
}

func TestUIRejectsUnknownFlags(t *testing.T) {
	if code := runUI([]string{"-nope"}); code != 2 {
		t.Errorf("runUI with an unknown flag = %d, want 2", code)
	}
}

// -h prints usage and exits rather than binding a port.
func TestUIHelpDoesNotServe(t *testing.T) {
	if code := runUI([]string{"-h"}); code != 2 {
		t.Errorf("runUI -h = %d, want 2", code)
	}
}
