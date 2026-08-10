package keystore

import "strings"

// redacted is what every Secret formatting path prints in place of the
// underlying value.
const redacted = "[redacted]"

// redactAll removes each of values from text, replacing every occurrence
// with the same placeholder Secret's own formatting uses. Drivers call it
// on text they did not author — the stderr of an operator-supplied command
// or driver executable — before putting it in an error that reaches the
// event stream, so surfacing that text for diagnosis cannot leak the very
// value being stored (KEY-003).
//
// A driver passes every encoding the secret travelled in, not just the raw
// bytes: an exec driver receives it base64-encoded, and a tool that echoes
// back what it was handed echoes back that form.
//
// The match is a plain substring one, which can over-redact a trivially
// short value — the safe direction, and not a case that arises for the
// high-entropy material init generates.
func redactAll(text string, values ...string) string {
	for _, value := range values {
		if value == "" {
			continue
		}
		text = strings.ReplaceAll(text, value, redacted)
	}
	return text
}

// Secret holds key material resolved by a Driver (KEY-003). The zero
// value is a valid, empty Secret. A string, not []byte, since Go strings
// are arbitrary byte sequences — binary key material round-trips through
// Reveal exactly.
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
