package keystore

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/driver"
)

var (
	_ Driver = execDriver{}
	_ Writer = writableExecDriver{}
)

// execDriver satisfies Driver for an out-of-tree keystore driver reached
// through the CORE-003 exec protocol: one process per call, method
// "resolve", params {"key": keyName}, result {"secret": <base64>}. Base64
// lets an executable return arbitrary binary key material (a certificate,
// a host key) through JSON, which is text-only.
//
// It is the resolve-only half of the driver: configured without
// config.store, an exec keystore deliberately does not implement Writer,
// so initialize.Run's type assertion fails at StepValidate rather than
// after an ACME round trip. writableExecDriver is what newExecDriver
// builds when config.store is true.
type execDriver struct {
	invoker driver.Invoker
}

type execResolveParams struct {
	Key string `json:"key"`
}

type execResolveResult struct {
	Secret string `json:"secret"`
}

type execStoreParams struct {
	Key    string `json:"key"`
	Secret string `json:"secret"`
}

// Resolve calls "resolve" and decodes the base64 secret the driver
// executable returns.
//
// A successful call carrying no secret is the protocol's positive "not
// found": the executable ran to completion, reported ok, and had no value
// to give, so Resolve wraps ErrNotFound. That distinction is load-bearing
// rather than cosmetic, and for the same reason it is in the command
// driver — guardedDriver.Store treats only ErrNotFound as "safe to write"
// (see keystore.go), so without it the guard would refuse every store of
// freshly minted non-rotating key material and init could never mint
// through an exec keystore at all. A failed call (nonzero exit, ok:false,
// unparseable response) stays a hard failure: a secret manager that is
// unreachable, unauthenticated, or simply broken must never read as an
// empty slot.
func (d execDriver) Resolve(ctx context.Context, keyName string) (Secret, error) {
	if strings.TrimSpace(keyName) == "" {
		return Secret{}, fmt.Errorf("keystore: exec: key name is required")
	}

	var result execResolveResult
	if err := d.invoker.Invoke(ctx, "resolve", execResolveParams{Key: keyName}, &result); err != nil {
		return Secret{}, fmt.Errorf("keystore: exec: resolve key %q: %w", keyName, err)
	}
	if strings.TrimSpace(result.Secret) == "" {
		return Secret{}, fmt.Errorf("keystore: exec: key %q not found: the driver returned no secret: %w", keyName, ErrNotFound)
	}
	secret, err := base64.StdEncoding.DecodeString(result.Secret)
	if err != nil {
		return Secret{}, fmt.Errorf("keystore: exec: resolve key %q: decode secret: %w", keyName, err)
	}
	if len(secret) == 0 {
		return Secret{}, fmt.Errorf("keystore: exec: key %q not found: the driver returned an empty secret: %w", keyName, ErrNotFound)
	}
	return NewSecret(string(secret)), nil
}

// writableExecDriver is execDriver plus the write side KEY-004 asks for:
// method "store", params {"key": keyName, "secret": <base64>}, empty
// result. It is what lets init mint a new instance's key material straight
// into an operator's own driver executable, instead of demanding the file
// driver and leaving a plaintext copy on disk.
//
// Whether this type or the resolve-only execDriver gets built is decided
// from config alone, before either exists — see newExecDriver.
type writableExecDriver struct {
	execDriver
}

// Store hands keyName's key material to the driver executable, base64 for
// the same reason Resolve decodes it: JSON carries text and key material
// is arbitrary bytes. The secret travels in the request body on the
// executable's stdin — never argv, never the environment (KEY-003), which
// driver.Exec guarantees by construction. A store call has no return
// value, so any result the executable sends back is discarded rather than
// inspected; success is the protocol's own ok:true.
//
// A failing executable's stderr reaches the error driver.Exec builds, and
// that error reaches the event stream, so the secret is scrubbed from it
// first — in both the raw and base64 forms it was exposed to, since a tool
// that echoes back what it was handed echoes back the encoded form. The
// scrubbed error deliberately does not wrap the transport error: an error
// whose message is redacted but whose chain still carries the value is not
// redacted, and nothing inspects a Store failure beyond reporting it.
func (d writableExecDriver) Store(ctx context.Context, keyName string, secret Secret) error {
	if strings.TrimSpace(keyName) == "" {
		return fmt.Errorf("keystore: exec: key name is required")
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(secret.Reveal()))
	if err := d.invoker.Invoke(ctx, "store", execStoreParams{Key: keyName, Secret: encoded}, nil); err != nil {
		return fmt.Errorf("keystore: exec: store key %q: %s", keyName, redactAll(err.Error(), secret.Reveal(), encoded))
	}
	return nil
}

// newExecDriver decides the driver's store capability from config and
// returns a type that either implements Writer or does not — never one
// that implements it and fails at call time. init type-asserts Writer at
// StepValidate (KEY-004), so a keystore that cannot accept key material is
// rejected before zone control is proven and before anything is generated.
//
// One execDriver type serves every driver executable, so the answer cannot
// come from the executable itself: a Store method implemented
// unconditionally would make every exec keystore pass that assertion, and
// resolve-only-ness would surface only once init called Store — after an
// ACME round trip and after key material already existed. config.store is
// therefore the operator's declaration that their executable implements
// the method, exactly as the presence of config.storeCommand is for the
// command driver.
//
// Declaring store: true against an executable that does not implement
// "store" still fails at the first call. That is the operator
// misconfiguring their own driver rather than a hole in the guarantee, and
// docs/tech-spec.md "Keystore driver config" records it as the one case
// config cannot catch — nothing here probes the executable to close it,
// since a probe would mean running a third-party binary during validation
// on the strength of guessing what a no-op call does.
func newExecDriver(driverName string, config map[string]any) (Driver, error) {
	path, err := stringConfig(config, "path")
	if err != nil {
		return nil, fmt.Errorf("keystore: %s: %w (unrecognized driver name, treated as an out-of-tree exec driver)", driverName, err)
	}

	var args []string
	if raw, ok := config["args"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("keystore: %s: config.args must be a list of strings", driverName)
		}
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("keystore: %s: config.args must be a list of strings", driverName)
			}
			args = append(args, s)
		}
	}

	store, err := optionalBoolConfig(config, "store")
	if err != nil {
		return nil, fmt.Errorf("keystore: %s: %w", driverName, err)
	}

	resolver := execDriver{invoker: driver.Exec{Path: path, Args: args}}
	if !store {
		return resolver, nil
	}
	return writableExecDriver{execDriver: resolver}, nil
}
