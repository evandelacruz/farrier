package forge

import "testing"

func TestRenderActionsSectionEnablesActions(t *testing.T) {
	got := RenderActionsSection()
	want := "[actions]\nENABLED = true\n"
	if got != want {
		t.Fatalf("RenderActionsSection() = %q, want %q", got, want)
	}
}
