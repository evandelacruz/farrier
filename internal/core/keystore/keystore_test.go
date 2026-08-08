package keystore

import "testing"

func TestNewRequiresDriverName(t *testing.T) {
	_, err := New("", map[string]any{})
	if err == nil {
		t.Fatal("New(\"\", ...) = nil error, want error")
	}
}

// var assertions, not a test: FileDriver implements Writer since its
// storage is an unambiguous directory on disk; CommandDriver deliberately
// does not, since KEY-002 defines it as read-only (the stdout of an
// operator-specified command has no generic notion of "write a secret
// here"). A caller that needs to generate and persist key material
// (initialize.Run, INIT-003) discovers this with a type assertion — if
// CommandDriver ever gained a Store method, that assertion would silently
// stop protecting the read-only guarantee KEY-002 promises.
var (
	_ Writer = FileDriver{}
	_ Driver = CommandDriver{}
)
