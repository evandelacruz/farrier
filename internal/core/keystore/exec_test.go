package keystore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// fakeInvoker plays the part of a driver executable in-process: it
// implements driver.Invoker directly against an in-memory store, so these
// tests exercise ExecResolver's wiring (params encoding, result decoding)
// without spawning a real subprocess — that's driver.Exec's own test, in
// internal/core/driver.
type fakeInvoker struct {
	store map[string]string
}

func newFakeInvoker() *fakeInvoker {
	return &fakeInvoker{store: map[string]string{}}
}

func (f *fakeInvoker) Invoke(ctx context.Context, method string, params, result any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}

	switch method {
	case "resolve":
		var p execResolveParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		secret, ok := f.store[p.Key]
		if !ok {
			return errors.New("fakeInvoker: unknown key " + p.Key)
		}
		out, err := json.Marshal(execResolveResult{Secret: secret})
		if err != nil {
			return err
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(out, result)
	default:
		return errors.New("fakeInvoker: unknown method " + method)
	}
}

func TestExecResolve(t *testing.T) {
	inv := newFakeInvoker()
	inv.store["ssh-host-key"] = "resolved-via-exec"

	r := NewExec(inv)
	got, err := r.Resolve(context.Background(), "ssh-host-key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := "resolved-via-exec"; got.Reveal() != want {
		t.Fatalf("Reveal() = %q, want %q", got.Reveal(), want)
	}
}

func TestExecResolveUnknownKeyErrors(t *testing.T) {
	r := NewExec(newFakeInvoker())
	if _, err := r.Resolve(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("Resolve: want error, got nil")
	}
}
