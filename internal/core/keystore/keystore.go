// Package keystore resolves key material at runtime through a driver: a
// keyName in, a redacted Secret out. The manifest's keystore DriverRef
// carries only a driver name and its non-secret config — a directory, a
// command, an executable to run — never the secret itself (CORE-001).
//
// Two drivers ship in-tree: file (KEY-001) and command (KEY-002). Any
// other driver name is reached through the CORE-003 exec protocol, so a
// third party can add a keystore driver as a standalone executable
// without linking Go — the same plugin posture as the dns and blob
// packages.
//
// Every Driver returns a Secret, never a bare string or []byte (KEY-003):
// Secret's formatting and marshaling methods all redact, so key material
// that ends up in an event Detail, a wrapped error, or a struct printed
// for debugging prints "[redacted]" instead of leaking. Reveal is the one
// deliberate escape hatch, used at the point the raw value is actually
// needed (e.g. rendering Compose environment or Forgejo's app.ini).
package keystore

import (
	"context"
	"fmt"
	"strings"
)

// Driver resolves one named piece of key material to its Secret.
// Implementations must never log, cache to disk, or otherwise persist a
// resolved secret anywhere outside memory (KEY-003).
type Driver interface {
	Resolve(ctx context.Context, keyName string) (Secret, error)
}

// Writer is the optional write side of a Driver: it stores a piece of key
// material under keyName, so it can later be read back through Resolve.
// Only drivers with an obvious, unambiguous place to put a secret implement
// it — FileDriver does, since its storage is just a directory on disk.
// CommandDriver deliberately does not: KEY-002 defines it as reading the
// stdout of an operator-specified command, a one-way interface with no
// generic notion of "write a secret here." A caller (initialize.Run,
// INIT-003) that needs to generate and persist key material checks for
// Writer with a type assertion and fails clearly when the configured
// driver doesn't implement it, rather than silently doing nothing.
type Writer interface {
	Store(ctx context.Context, keyName string, secret Secret) error
}

// New builds the Driver named by driverName from its non-secret config, as
// carried by a bundle manifest's keystore DriverRef. "file" and "command"
// are the shipped in-tree drivers (KEY-001, KEY-002); any other name is
// treated as an out-of-tree driver executable reached through the
// CORE-003 exec protocol, configured the same way driver.Exec itself is:
// config.path is the executable, config.args its fixed arguments.
func New(driverName string, config map[string]any) (Driver, error) {
	switch driverName {
	case "":
		return nil, fmt.Errorf("keystore: driver name is required")
	case "file":
		return newFileDriver(config)
	case "command":
		return newCommandDriver(config)
	default:
		return newExecDriver(driverName, config)
	}
}

// stringConfig reads a required, non-empty string field out of a driver's
// config map, naming the field in any error so a bad manifest is
// diagnosable without a debugger.
func stringConfig(config map[string]any, field string) (string, error) {
	raw, ok := config[field]
	if !ok {
		return "", fmt.Errorf("config.%s is required", field)
	}
	s, ok := raw.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("config.%s must be a non-empty string", field)
	}
	return s, nil
}
