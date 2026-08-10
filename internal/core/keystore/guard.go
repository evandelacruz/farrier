package keystore

import (
	"context"
	"errors"
	"fmt"
)

// guardedDriver wraps a Driver's optional Writer with the rotation-policy
// guard every driver New builds gets automatically, regardless of which
// driver is underneath. It lives here — above the driver, never inside one
// — because an out-of-tree driver is a separate executable speaking JSON
// over stdin/stdout (CORE-003) and cannot consult a Go registry, and
// because a third-party driver author should not have to implement
// "refuse overwrite" to be conformant: the domain policy stays in core.
type guardedDriver struct {
	Driver
	writer Writer
}

// Store refuses to overwrite key material that Rotates says must not
// rotate when Resolve reports it already has content — the single
// invariant that used to live inside FileDriver.Store, now enforced once
// for every driver a keystore target might use. Rotates defaults a name
// nobody has declared to non-rotating, so newly added key material is
// protected from the start rather than opting in; a key material this
// blocks the first time (nothing yet at keyName) succeeds normally.
//
// A Resolve error is only ever treated as "safe to write" when it
// satisfies errors.Is(err, ErrNotFound) — a positive "nothing here yet."
// Any other Resolve error (permission denied, an I/O error, a timeout or
// malformed response from a CORE-003 exec driver) means the check itself
// failed, not that the key is absent, so Store refuses and reports rather
// than falling through to a write that could silently overwrite
// non-rotating key material (SECRET_KEY, INTERNAL_TOKEN, the SSH host
// key, ...).
func (g guardedDriver) Store(ctx context.Context, keyName string, secret Secret) error {
	if !Rotates(keyName) {
		switch _, err := g.Driver.Resolve(ctx, keyName); {
		case err == nil:
			return fmt.Errorf("keystore: key %q already exists and is not declared rotating, refusing to overwrite", keyName)
		case !errors.Is(err, ErrNotFound):
			return fmt.Errorf("keystore: key %q: could not confirm no existing value before store: %w", keyName, err)
		}
	}
	return g.writer.Store(ctx, keyName, secret)
}

// DescribeTarget forwards to the wrapped driver when that driver
// describes itself, so wrapping it in the rotation guard never costs a
// caller the location report INIT-006 depends on. A wrapped driver that
// is not a Describer answers with an empty string — the same "unknown
// here" Target reads from a bare driver.
func (g guardedDriver) DescribeTarget(keyName string) string {
	if describer, ok := g.Driver.(Describer); ok {
		return describer.DescribeTarget(keyName)
	}
	return ""
}

var (
	_ Driver    = guardedDriver{}
	_ Writer    = guardedDriver{}
	_ Describer = guardedDriver{}
)
