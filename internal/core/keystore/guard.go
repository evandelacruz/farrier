package keystore

import (
	"context"
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
func (g guardedDriver) Store(ctx context.Context, keyName string, secret Secret) error {
	if !Rotates(keyName) {
		if _, err := g.Driver.Resolve(ctx, keyName); err == nil {
			return fmt.Errorf("keystore: key %q already exists and is not declared rotating, refusing to overwrite", keyName)
		}
	}
	return g.writer.Store(ctx, keyName, secret)
}

var (
	_ Driver = guardedDriver{}
	_ Writer = guardedDriver{}
)
