package dns

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeInvoker is a driver.Invoker test double that records the call it
// received and serves a canned error, without spawning a process — the
// same shape keystore's and blob's exec tests use.
type fakeInvoker struct {
	gotMethod string
	gotParams any
	err       error
}

func (f *fakeInvoker) Invoke(ctx context.Context, method string, params, result any) error {
	f.gotMethod = method
	f.gotParams = params
	return f.err
}

func TestExecDriverSetInvokesSetWithSeconds(t *testing.T) {
	fake := &fakeInvoker{}
	d := NewExec(fake)

	if err := d.Set(context.Background(), "app.example.com", "203.0.113.10", 90*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if fake.gotMethod != "set" {
		t.Fatalf("method = %q, want %q", fake.gotMethod, "set")
	}
	params, ok := fake.gotParams.(execSetParams)
	if !ok {
		t.Fatalf("params = %T, want execSetParams", fake.gotParams)
	}
	want := execSetParams{Record: "app.example.com", Value: "203.0.113.10", TTL: 90}
	if params != want {
		t.Fatalf("params = %+v, want %+v", params, want)
	}
}

func TestExecDriverDeleteInvokesDelete(t *testing.T) {
	fake := &fakeInvoker{}
	d := NewExec(fake)

	if err := d.Delete(context.Background(), "app.example.com"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if fake.gotMethod != "delete" {
		t.Fatalf("method = %q, want %q", fake.gotMethod, "delete")
	}
	params, ok := fake.gotParams.(execDeleteParams)
	if !ok || params.Record != "app.example.com" {
		t.Fatalf("params = %+v, want Record=app.example.com", fake.gotParams)
	}
}

func TestExecDriverInvokerErrorPropagates(t *testing.T) {
	fake := &fakeInvoker{err: errors.New("boom")}
	d := NewExec(fake)

	err := d.Set(context.Background(), "app.example.com", "203.0.113.10", 60*time.Second)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Set error = %v, want it to contain %q", err, "boom")
	}
}

func TestExecDriverSetValidatesArgsBeforeInvoking(t *testing.T) {
	fake := &fakeInvoker{}
	d := NewExec(fake)

	if err := d.Set(context.Background(), "", "203.0.113.10", 60*time.Second); err == nil {
		t.Fatal("Set with empty record: want error, got nil")
	}
	if fake.gotMethod != "" {
		t.Fatal("Set: invoker should not be called for an empty record")
	}
}

// ensure execSetParams round-trips through JSON the way driver.Exec would
// encode it, i.e. as {"record","value","ttl"}.
func TestExecSetParamsJSONShape(t *testing.T) {
	encoded, err := json.Marshal(execSetParams{Record: "r", Value: "v", TTL: 60})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"ttl":60`) {
		t.Fatalf("encoded = %s, want ttl field", encoded)
	}
}
