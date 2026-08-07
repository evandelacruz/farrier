package keystore

import "testing"

func TestNewRequiresDriverName(t *testing.T) {
	_, err := New("", map[string]any{})
	if err == nil {
		t.Fatal("New(\"\", ...) = nil error, want error")
	}
}
