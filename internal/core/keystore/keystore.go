// Package keystore implements the KEY driver family: the abstraction key
// material resolves through at runtime instead of living in the bundle
// (spec.md "Bundle config is shareable; keys resolve through drivers").
//
// Resolver is the published Go interface every keystore driver satisfies.
// Two ship in-tree: file (KEY-001), reading a local path, and command
// (KEY-002), reading a configured command's stdout. ExecResolver satisfies
// it too, reaching a third-party driver through the CORE-003 exec protocol
// — the same posture used by dns and blob.
//
// Secret is KEY-003's enforcement point. Every Resolver returns a Secret,
// never a string, and Secret's zero-value String, GoString, MarshalJSON,
// and MarshalYAML all redact — so a Secret that ends up in an event
// Detail, a wrapped error, or a struct printed for debugging prints
// "[redacted]" instead of the key material. Reveal is the one deliberate
// escape hatch, used at the point the raw value is actually needed (e.g.
// rendering Compose environment or Forgejo's app.ini).
package keystore

import "context"

// Resolver resolves named key material at runtime.
type Resolver interface {
	// Resolve returns the key material named by keyName.
	Resolve(ctx context.Context, keyName string) (Secret, error)
}

// redacted is what every Secret formatting path prints in place of the
// underlying value.
const redacted = "[redacted]"

// Secret holds key material resolved by a Resolver. The zero value is a
// valid, empty Secret.
type Secret struct {
	value string
}

// NewSecret wraps value as a Secret.
func NewSecret(value string) Secret {
	return Secret{value: value}
}

// Reveal returns the underlying key material. Callers use it only at the
// point the raw value is actually needed — never to build a log line, an
// event Detail, or an error message.
func (s Secret) Reveal() string {
	return s.value
}

// String implements fmt.Stringer, redacting the value so a Secret printed
// via %v, %s, or an ordinary Println never surfaces key material — even
// nested inside another struct's fields.
func (s Secret) String() string {
	return redacted
}

// GoString implements fmt.GoStringer, redacting the value under %#v.
func (s Secret) GoString() string {
	return redacted
}

// MarshalJSON redacts the value, so a Secret embedded in any struct
// marshaled to JSON — for logging, an API response, or debugging output —
// never carries the key material.
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"` + redacted + `"`), nil
}

// MarshalYAML redacts the value, matching MarshalJSON for the YAML encoder
// the bundle manifest uses (gopkg.in/yaml.v3).
func (s Secret) MarshalYAML() (any, error) {
	return redacted, nil
}
