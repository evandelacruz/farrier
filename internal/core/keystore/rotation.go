package keystore

// KeyTLSCertificate and KeyTLSPrivateKey name the one piece of bundle key
// material that must rotate (ACME-002): a TLS certificate expires and has
// to be replaced before it does. Every other piece of key material is
// bundle identity and must never silently change once init writes it
// (spec.md "Identity" > "Key material"). The values match
// initialize.KeyTLSCertificate/KeyTLSPrivateKey and
// state.KeyTLSCertificate/KeyTLSPrivateKey exactly; declared again here,
// the same house pattern state.go uses, so this package doesn't import
// initialize or state just to name two strings.
const (
	KeyTLSCertificate = "tls_certificate"
	KeyTLSPrivateKey  = "tls_private_key"
)

// rotating is the fail-closed rotation registry Store consults: a key name
// present and true here may be overwritten at any time; every other key
// name — including one nobody has registered yet — is treated as
// non-rotating, so new key material added later is protected automatically
// rather than left to opt in.
var rotating = map[string]bool{
	KeyTLSCertificate: true,
	KeyTLSPrivateKey:  true,
}

// Rotates reports whether keyName is declared rotation-eligible. A name
// absent from the registry defaults to false — non-rotating — the
// fail-closed direction spec.md "Identity" requires: Forgejo's SECRET_KEY
// and INTERNAL_TOKEN, the LFS JWT secret, SSH host keys, and the age
// backup key must never rotate, and none of them needs an entry here to
// stay protected.
func Rotates(keyName string) bool {
	return rotating[keyName]
}
