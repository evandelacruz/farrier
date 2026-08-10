package keystore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/driver"
)

// fakeInvoker is a driver.Invoker test double that records the call it
// received and serves a canned result or error, without spawning a
// process.
type fakeInvoker struct {
	gotMethod string
	gotParams any
	result    any
	err       error
}

func (f *fakeInvoker) Invoke(ctx context.Context, method string, params, result any) error {
	f.gotMethod = method
	f.gotParams = params
	if f.err != nil {
		return f.err
	}
	// A nil result discards the call's return value, per Invoker — which is
	// what a store call passes, since the method has no result.
	if result == nil {
		return nil
	}
	data, err := json.Marshal(f.result)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, result)
}

func TestExecDriverResolveDecodesBase64Secret(t *testing.T) {
	secret := []byte("hunter2")
	fake := &fakeInvoker{result: execResolveResult{Secret: base64.StdEncoding.EncodeToString(secret)}}
	d := execDriver{invoker: fake}

	got, err := d.Resolve(context.Background(), "forgejo_secret_key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Reveal() != "hunter2" {
		t.Fatalf("Reveal() = %q, want %q", got.Reveal(), "hunter2")
	}
	if fake.gotMethod != "resolve" {
		t.Fatalf("method = %q, want %q", fake.gotMethod, "resolve")
	}
	params, ok := fake.gotParams.(execResolveParams)
	if !ok || params.Key != "forgejo_secret_key" {
		t.Fatalf("params = %+v, want Key=forgejo_secret_key", fake.gotParams)
	}
}

func TestExecDriverResolveInvokerErrorPropagates(t *testing.T) {
	fake := &fakeInvoker{err: errors.New("boom")}
	d := execDriver{invoker: fake}

	_, err := d.Resolve(context.Background(), "k")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Resolve error = %v, want it to contain %q", err, "boom")
	}
}

func TestExecDriverResolveInvalidBase64Errors(t *testing.T) {
	fake := &fakeInvoker{result: execResolveResult{Secret: "not-valid-base64!!"}}
	d := execDriver{invoker: fake}

	if _, err := d.Resolve(context.Background(), "k"); err == nil {
		t.Fatal("Resolve: want error for invalid base64, got nil")
	}
}

// An ok response carrying no secret is the protocol's positive "not
// found" — the answer guardedDriver.Store requires before it will write
// non-rotating key material.
func TestExecDriverResolveEmptySecretIsNotFound(t *testing.T) {
	fake := &fakeInvoker{result: execResolveResult{Secret: ""}}
	d := execDriver{invoker: fake}

	_, err := d.Resolve(context.Background(), "k")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve error = %v, want it to wrap ErrNotFound", err)
	}
}

// A failed call is never an absence: an unreachable or broken driver must
// not read as an empty slot the rotation guard would happily write over.
func TestExecDriverResolveFailureIsNotNotFound(t *testing.T) {
	fake := &fakeInvoker{err: errors.New("vault sealed")}
	d := execDriver{invoker: fake}

	_, err := d.Resolve(context.Background(), "k")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve error = %v, want a non-ErrNotFound failure", err)
	}
}

// The guard's fail-closed lookup and the exec driver's not-found have to
// agree, or init can never mint non-rotating key material through an
// out-of-tree driver — the whole point of the store method.
func TestGuardedExecDriverStoresWhenKeyAbsent(t *testing.T) {
	fake := &fakeInvoker{result: execResolveResult{Secret: ""}}
	d := guardedDriver{Driver: writableExecDriver{execDriver{invoker: fake}}, writer: writableExecDriver{execDriver{invoker: fake}}}

	if Rotates("forgejo_secret_key") {
		t.Fatal("test premise: forgejo_secret_key must be non-rotating")
	}
	if err := d.Store(context.Background(), "forgejo_secret_key", NewSecret("hunter2")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if fake.gotMethod != "store" {
		t.Fatalf("method = %q, want the guard to have reached store", fake.gotMethod)
	}
}

func TestExecDriverResolveEmptyKeyNameErrors(t *testing.T) {
	fake := &fakeInvoker{}
	d := execDriver{invoker: fake}

	if _, err := d.Resolve(context.Background(), ""); err == nil {
		t.Fatal("Resolve: want error for empty key name, got nil")
	}
	if fake.gotMethod != "" {
		t.Fatal("Resolve: invoker should not be called for an empty key name")
	}
}

func TestNewExecDriverRequiresPath(t *testing.T) {
	if _, err := New("onepassword", map[string]any{}); err == nil {
		t.Fatal("New: want error for missing config.path, got nil")
	}
}

func TestNewExecDriverParsesPathAndArgs(t *testing.T) {
	got, err := New("onepassword", map[string]any{
		"path": "/usr/local/bin/farrier-keystore-onepassword",
		"args": []any{"--vault", "prod"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ed, ok := got.(execDriver)
	if !ok {
		t.Fatalf("New = %T, want execDriver", got)
	}
	exe, ok := ed.invoker.(driver.Exec)
	if !ok {
		t.Fatalf("execDriver.invoker = %T, want driver.Exec", ed.invoker)
	}
	if exe.Path != "/usr/local/bin/farrier-keystore-onepassword" {
		t.Fatalf("Path = %q, want %q", exe.Path, "/usr/local/bin/farrier-keystore-onepassword")
	}
	if len(exe.Args) != 2 || exe.Args[0] != "--vault" || exe.Args[1] != "prod" {
		t.Fatalf("Args = %v, want [--vault prod]", exe.Args)
	}
}

func TestNewExecDriverRejectsNonStringArgs(t *testing.T) {
	_, err := New("onepassword", map[string]any{
		"path": "/usr/local/bin/farrier-keystore-onepassword",
		"args": []any{1, 2},
	})
	if err == nil {
		t.Fatal("New: want error for non-string args, got nil")
	}
}

func TestExecDriverStoreEncodesSecretAsBase64(t *testing.T) {
	fake := &fakeInvoker{}
	d := writableExecDriver{execDriver{invoker: fake}}

	if err := d.Store(context.Background(), "forgejo_secret_key", NewSecret("hunter2")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if fake.gotMethod != "store" {
		t.Fatalf("method = %q, want %q", fake.gotMethod, "store")
	}
	params, ok := fake.gotParams.(execStoreParams)
	if !ok {
		t.Fatalf("params = %+v, want execStoreParams", fake.gotParams)
	}
	if params.Key != "forgejo_secret_key" {
		t.Fatalf("params.Key = %q, want %q", params.Key, "forgejo_secret_key")
	}
	if want := base64.StdEncoding.EncodeToString([]byte("hunter2")); params.Secret != want {
		t.Fatalf("params.Secret = %q, want %q", params.Secret, want)
	}
}

// Binary key material — an SSH host key, a certificate — is why the wire
// format is base64 rather than a plain JSON string, so it has to survive
// the round trip byte for byte.
func TestExecDriverStoreRoundTripsBinarySecret(t *testing.T) {
	binary := string([]byte{0x00, 0xff, 0x0a, 0x80, 0x7f})
	fake := &fakeInvoker{}
	d := writableExecDriver{execDriver{invoker: fake}}

	if err := d.Store(context.Background(), "ssh_host_key", NewSecret(binary)); err != nil {
		t.Fatalf("Store: %v", err)
	}
	params := fake.gotParams.(execStoreParams)
	decoded, err := base64.StdEncoding.DecodeString(params.Secret)
	if err != nil {
		t.Fatalf("decode params.Secret: %v", err)
	}
	if string(decoded) != binary {
		t.Fatalf("decoded secret = %q, want %q", decoded, binary)
	}
}

func TestExecDriverStoreEmptyKeyNameErrors(t *testing.T) {
	fake := &fakeInvoker{}
	d := writableExecDriver{execDriver{invoker: fake}}

	if err := d.Store(context.Background(), "", NewSecret("hunter2")); err == nil {
		t.Fatal("Store: want error for empty key name, got nil")
	}
	if fake.gotMethod != "" {
		t.Fatal("Store: invoker should not be called for an empty key name")
	}
}

func TestExecDriverStoreInvokerErrorPropagates(t *testing.T) {
	fake := &fakeInvoker{err: errors.New("vault sealed")}
	d := writableExecDriver{execDriver{invoker: fake}}

	err := d.Store(context.Background(), "k", NewSecret("hunter2"))
	if err == nil || !strings.Contains(err.Error(), "vault sealed") {
		t.Fatalf("Store error = %v, want it to contain %q", err, "vault sealed")
	}
}

// A driver executable that echoes what it was handed puts the secret in
// the stderr driver.Exec surfaces, and that error reaches the event
// stream. Both the raw and base64 forms have to be scrubbed (KEY-003).
func TestExecDriverStoreRedactsSecretFromError(t *testing.T) {
	secret := NewSecret("hunter2")
	encoded := base64.StdEncoding.EncodeToString([]byte("hunter2"))
	fake := &fakeInvoker{err: errors.New("refused to write hunter2 (encoded " + encoded + ")")}
	d := writableExecDriver{execDriver{invoker: fake}}

	err := d.Store(context.Background(), "k", secret)
	if err == nil {
		t.Fatal("Store: want error, got nil")
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), encoded) {
		t.Fatalf("Store error leaks key material: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "refused to write") {
		t.Fatalf("Store error = %q, want the driver's diagnostic preserved", err.Error())
	}
}

// The scrubbed error must not wrap the transport error either: a chain
// whose message is redacted but whose links still carry the value is not
// redacted.
func TestExecDriverStoreErrorChainCarriesNoSecret(t *testing.T) {
	leak := errors.New("wrote hunter2")
	d := writableExecDriver{execDriver{invoker: &fakeInvoker{err: leak}}}

	err := d.Store(context.Background(), "k", NewSecret("hunter2"))
	if errors.Is(err, leak) {
		t.Fatal("Store error wraps the unredacted transport error")
	}
}

func TestNewExecDriverWithoutStoreIsResolveOnly(t *testing.T) {
	got, err := New("onepassword", map[string]any{"path": "/usr/local/bin/op-keystore"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := got.(Writer); ok {
		t.Fatal("New: an exec driver without config.store must not implement Writer")
	}
}

func TestNewExecDriverStoreFalseIsResolveOnly(t *testing.T) {
	got, err := New("onepassword", map[string]any{
		"path":  "/usr/local/bin/op-keystore",
		"store": false,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := got.(Writer); ok {
		t.Fatal("New: config.store false must not implement Writer")
	}
}

func TestNewExecDriverStoreTrueImplementsWriter(t *testing.T) {
	got, err := New("onepassword", map[string]any{
		"path":  "/usr/local/bin/op-keystore",
		"store": true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := got.(Writer); !ok {
		t.Fatalf("New = %T, want it to implement Writer", got)
	}
	// New wraps a writable driver in the rotation guard, so the driver a
	// caller stores through is guarded like every other one.
	guarded, ok := got.(guardedDriver)
	if !ok {
		t.Fatalf("New = %T, want guardedDriver", got)
	}
	if _, ok := guarded.Driver.(writableExecDriver); !ok {
		t.Fatalf("guarded driver = %T, want writableExecDriver", guarded.Driver)
	}
}

func TestNewExecDriverRejectsNonBoolStore(t *testing.T) {
	_, err := New("onepassword", map[string]any{
		"path":  "/usr/local/bin/op-keystore",
		"store": "true",
	})
	if err == nil {
		t.Fatal("New: want error for a non-boolean config.store, got nil")
	}
}
