// Package keystore resolves key material at runtime through a driver: a
// keyName in, a redacted Secret out. The manifest's keystore DriverRef
// carries only a driver name and its non-secret config — a directory, a
// command, an executable to run — never the secret itself (CORE-001).
//
// Some drivers also write, through the optional Writer side, which is
// what lets init mint a new instance's key material into the operator's
// own secret manager instead of a file on disk. Writer is a
// construction-time fact, not a call-time one — see Writer.
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
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound is the sentinel a Driver's Resolve must wrap (via %w, so
// errors.Is sees it) when — and only when — it has positively determined
// that keyName has no key material yet. Any other Resolve error (a
// permission error, an I/O error, a timeout or malformed response from a
// CORE-003 exec driver) means resolution itself failed and must not be
// mistaken for "nothing here yet, safe to write": guardedDriver.Store
// (guard.go) relies on exactly this distinction to stay fail-closed for
// non-rotating key material, and inferring it from an error string or
// treating every error alike would silently defeat that guarantee.
var ErrNotFound = errors.New("keystore: key not found")

// Driver resolves one named piece of key material to its Secret.
// Implementations must never log, cache to disk, or otherwise persist a
// resolved secret anywhere outside memory (KEY-003). Resolve must return
// an error satisfying errors.Is(err, ErrNotFound) when keyName is
// positively absent, and a different (non-ErrNotFound) error for any other
// failure — see ErrNotFound.
type Driver interface {
	Resolve(ctx context.Context, keyName string) (Secret, error)
}

// Writer is the optional write side of a Driver: it stores a piece of key
// material under keyName, so it can later be read back through Resolve.
// Only a driver that has been told where to put a secret implements it.
// FileDriver always does, since its storage is just a directory on disk;
// the command driver does only when the operator configured a
// storeCommand, in which case New builds a WritableCommandDriver rather
// than a CommandDriver.
//
// Whether a driver implements Writer is therefore settled when the driver
// is built, never at the moment of the call (KEY-004). A caller
// (initialize.Run, INIT-003) that needs to generate and persist key
// material type-asserts Writer during validation and fails clearly there —
// before it proves zone control or generates anything — instead of
// discovering a read-only keystore once key material already exists.
type Writer interface {
	Store(ctx context.Context, keyName string, secret Secret) error
}

// New builds the Driver named by driverName from its non-secret config, as
// carried by a bundle manifest's keystore DriverRef. "file" and "command"
// are the shipped in-tree drivers (KEY-001, KEY-002); any other name is
// treated as an out-of-tree driver executable reached through the
// CORE-003 exec protocol, configured the same way driver.Exec itself is:
// config.path is the executable, config.args its fixed arguments.
//
// When the built driver implements Writer, New wraps it in the rotation
// guard (guardedDriver) before returning it, so every caller's Store goes
// through the same policy check no matter which driver they configured —
// a caller that type-asserts the result for Writer still sees one, since
// guardedDriver implements it too.
func New(driverName string, config map[string]any) (Driver, error) {
	d, err := newDriver(driverName, config)
	if err != nil {
		return nil, err
	}
	if w, ok := d.(Writer); ok {
		return guardedDriver{Driver: d, writer: w}, nil
	}
	return d, nil
}

func newDriver(driverName string, config map[string]any) (Driver, error) {
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

// optionalStringConfig reads a string field a driver treats as optional,
// reporting whether it was present at all. A field that is present but
// blank or not a string is an error rather than a silent absence: presence
// is what decides a driver's store capability (tech-spec "Keystore driver
// config"), so downgrading a typo'd or empty storeCommand to "resolve-only"
// would turn a one-character manifest mistake into a confusing "this
// keystore cannot store key material" much later.
func optionalStringConfig(config map[string]any, field string) (string, bool, error) {
	if _, present := config[field]; !present {
		return "", false, nil
	}
	s, err := stringConfig(config, field)
	if err != nil {
		return "", false, err
	}
	return s, true, nil
}
